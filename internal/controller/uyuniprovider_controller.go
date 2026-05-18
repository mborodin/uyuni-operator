package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type UyuniProviderReconciler struct {
	client.Client
	Pool       uyuni.ClientPool
	OperatorNS string
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=uyuniproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=uyuniproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *UyuniProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var prov uyuniv1.UyuniProvider
	if err := r.Get(ctx, req.NamespacedName, &prov); err != nil {
		// Provider deleted — evict from pool. No finalizer needed since
		// there's no Uyuni-side state to clean up.
		r.Pool.Invalidate(req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Race-window backstop: webhook rejects duplicate defaults at admission,
	// but two simultaneous creates can both pass admission before either lands
	// in etcd. The reconciler catches the race.
	if prov.Spec.IsDefault {
		if dup, err := r.findOtherDefault(ctx, &prov); err != nil {
			return ctrl.Result{}, err
		} else if dup != "" {
			setReady(&prov.Status.Conditions, prov.Generation, metav1.ConditionFalse,
				"DuplicateDefault",
				fmt.Sprintf("UyuniProvider %q is also marked default; resolve by removing isDefault on one", dup))
			return ctrl.Result{}, r.Status().Update(ctx, &prov)
		}
	}

	uc, err := r.Pool.For(ctx, &uyuni.LocalObjectRef{Name: prov.Name}, "")
	if err != nil {
		setReady(&prov.Status.Conditions, prov.Generation, metav1.ConditionFalse,
			"Unreachable", err.Error())
		_ = r.Status().Update(ctx, &prov)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	version, err := uc.GetServerVersion(ctx)
	if err != nil {
		setReady(&prov.Status.Conditions, prov.Generation, metav1.ConditionFalse,
			"VersionProbeFailed", err.Error())
		_ = r.Status().Update(ctx, &prov)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	orgID, _ := r.Pool.OrgID(prov.Name)

	now := metav1.NewTime(time.Now())
	prov.Status.ServerVersion = version
	prov.Status.OrgID = orgID
	prov.Status.LastReachableTime = &now
	prov.Status.ObservedGeneration = prov.Generation
	setReady(&prov.Status.Conditions, prov.Generation, metav1.ConditionTrue, "Reachable", "")
	if err := r.Status().Update(ctx, &prov); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

func (r *UyuniProviderReconciler) findOtherDefault(ctx context.Context, prov *uyuniv1.UyuniProvider) (string, error) {
	var list uyuniv1.UyuniProviderList
	if err := r.List(ctx, &list); err != nil {
		return "", err
	}
	for _, other := range list.Items {
		if other.Name != prov.Name && other.Spec.IsDefault {
			return other.Name, nil
		}
	}
	return "", nil
}

func (r *UyuniProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.UyuniProvider{}).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.providersForSecret),
			builder.WithPredicates(secretInNamespace(r.OperatorNS))).
		Complete(r)
}

func (r *UyuniProviderReconciler) providersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNS {
		return nil
	}
	var list uyuniv1.UyuniProviderList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, prov := range list.Items {
		refs := []corev1.SecretReference{prov.Spec.CredentialsSecretRef}
		if prov.Spec.CACertSecretRef != nil {
			refs = append(refs, *prov.Spec.CACertSecretRef)
		}
		for _, ref := range refs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: prov.Name},
				})
				r.Pool.Invalidate(prov.Name)
				break
			}
		}
	}
	return out
}

func secretInNamespace(ns string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == ns
	})
}
