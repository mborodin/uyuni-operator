package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type CustomInfoKeyReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=custominfokeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=custominfokeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=custominfokeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch

func (r *CustomInfoKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cik uyuniv1.CustomInfoKey
	if err := r.Get(ctx, req.NamespacedName, &cik); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cik.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &cik)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(cik.Spec.OrganizationRef), cik.Namespace)
	if err != nil {
		return r.fail(ctx, &cik, "OrganizationError", err)
	}

	if ensureFinalizer(&cik, cikFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cik)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &cik, orgRef(cik.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	existing, err := r.findKey(ctx, uc, cik.Spec.Label)
	if err != nil {
		return r.fail(ctx, &cik, "ProviderError", err)
	}
	if existing == nil {
		if err := uc.CreateCustomInfoKey(ctx, cik.Spec.Label, cik.Spec.Description); err != nil {
			return r.fail(ctx, &cik, "CreateFailed", err)
		}
		// createKey returns success, not the new ID — re-list to capture it.
		existing, _ = r.findKey(ctx, uc, cik.Spec.Label)
	} else if existing.Description != cik.Spec.Description {
		if err := uc.UpdateCustomInfoKey(ctx, cik.Spec.Label, cik.Spec.Description); err != nil {
			return r.fail(ctx, &cik, "UpdateFailed", err)
		}
	}

	if existing != nil {
		cik.Status.UyuniID = existing.ID
	}
	cik.Status.ObservedGeneration = cik.Generation
	setReady(&cik.Status.Conditions, cik.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &cik); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *CustomInfoKeyReconciler) handleDeletion(ctx context.Context, cik *uyuniv1.CustomInfoKey) (ctrl.Result, error) {
	if !containsFinalizer(cik, cikFinalizer) {
		return ctrl.Result{}, nil
	}
	if cik.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(cik, cikFinalizer)
		return ctrl.Result{}, r.Update(ctx, cik)
	}

	// InUse guard: block deletion while any System still references this key.
	// Deleting the key in Uyuni would drop its values from every system.
	refs, err := r.referencingSystems(ctx, cik)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(refs) > 0 {
		setReady(&cik.Status.Conditions, cik.Generation, metav1.ConditionFalse,
			"InUse", fmt.Sprintf("custom info key is referenced by System(s): %v", refs))
		_ = r.Status().Update(ctx, cik)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(cik.Spec.OrganizationRef), cik.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := uc.DeleteCustomInfoKey(ctx, cik.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(cik, cikFinalizer)
	return ctrl.Result{}, r.Update(ctx, cik)
}

func (r *CustomInfoKeyReconciler) findKey(ctx context.Context, uc uyuni.API, label string) (*uyuni.CustomInfoKeyDetails, error) {
	keys, err := uc.ListCustomInfoKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].Label == label {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// referencingSystems returns the names of System CRs in the same namespace
// that reference this key via spec.customInfoValues[].keyRef.
func (r *CustomInfoKeyReconciler) referencingSystems(ctx context.Context, cik *uyuniv1.CustomInfoKey) ([]string, error) {
	var list uyuniv1.SystemList
	if err := r.List(ctx, &list, client.InNamespace(cik.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, sys := range list.Items {
		for _, v := range sys.Spec.CustomInfoValues {
			if v.KeyRef.Name == cik.Name {
				out = append(out, sys.Name)
				break
			}
		}
	}
	return out, nil
}

func (r *CustomInfoKeyReconciler) fail(ctx context.Context, cik *uyuniv1.CustomInfoKey, reason string, err error) (ctrl.Result, error) {
	setReady(&cik.Status.Conditions, cik.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, cik)
	return ctrl.Result{}, err
}

func (r *CustomInfoKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.CustomInfoKey{}).
		Watches(&uyuniv1.System{},
			handler.EnqueueRequestsFromMapFunc(r.keysForSystem)).
		Complete(r)
}

func (r *CustomInfoKeyReconciler) keysForSystem(ctx context.Context, obj client.Object) []reconcile.Request {
	sys, ok := obj.(*uyuniv1.System)
	if !ok {
		return nil
	}
	var out []reconcile.Request
	for _, v := range sys.Spec.CustomInfoValues {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: v.KeyRef.Name},
		})
	}
	return out
}
