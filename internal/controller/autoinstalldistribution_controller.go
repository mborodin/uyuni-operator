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

type AutoinstallDistributionReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstalldistributions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstalldistributions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstalldistributions/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels,verbs=get;list;watch

func (r *AutoinstallDistributionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ad uyuniv1.AutoinstallDistribution
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ad.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ad)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ad.Spec.OrganizationRef), ad.Namespace)
	if err != nil {
		return r.fail(ctx, &ad, "OrganizationError", err)
	}

	if ensureFinalizer(&ad, adFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ad)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &ad, orgRef(ad.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve channel label from spec.channelRef.
	channelLabel, waitReason, err := r.resolveChannelLabel(ctx, &ad)
	if err != nil {
		return r.fail(ctx, &ad, "ResolveChannelFailed", err)
	}
	if waitReason != "" {
		setReady(&ad.Status.Conditions, ad.Generation, metav1.ConditionFalse, "WaitingForChannel", waitReason)
		_ = r.Status().Update(ctx, &ad)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	current, err := uc.GetDistribution(ctx, ad.Spec.Label)
	if uyuni.IsNotFound(err) {
		if createErr := uc.CreateDistribution(ctx, uyuni.DistributionDetails{
			Label:             ad.Spec.Label,
			BasePath:          ad.Spec.BasePath,
			ChannelLabel:      channelLabel,
			InstallType:       ad.Spec.InstallType,
			KernelOptions:     ad.Spec.KernelOptions,
			PostKernelOptions: ad.Spec.PostKernelOptions,
		}); createErr != nil {
			return r.fail(ctx, &ad, "CreateFailed", createErr)
		}
		current, err = uc.GetDistribution(ctx, ad.Spec.Label)
		if err != nil {
			return r.fail(ctx, &ad, "GetAfterCreate", err)
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Sync mutable fields if drifted.
	if distributionNeedsUpdate(current, &ad, channelLabel) {
		if updateErr := uc.UpdateDistribution(ctx, ad.Spec.Label, uyuni.DistributionDetails{
			Label:             ad.Spec.Label,
			BasePath:          ad.Spec.BasePath,
			ChannelLabel:      channelLabel,
			InstallType:       ad.Spec.InstallType,
			KernelOptions:     ad.Spec.KernelOptions,
			PostKernelOptions: ad.Spec.PostKernelOptions,
		}); updateErr != nil {
			return r.fail(ctx, &ad, "UpdateFailed", updateErr)
		}
	}

	ad.Status.UyuniID = current.ID
	ad.Status.ChannelLabel = channelLabel
	ad.Status.ObservedGeneration = ad.Generation
	setReady(&ad.Status.Conditions, ad.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &ad); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *AutoinstallDistributionReconciler) handleDeletion(ctx context.Context, ad *uyuniv1.AutoinstallDistribution) (ctrl.Result, error) {
	if !containsFinalizer(ad, adFinalizer) {
		return ctrl.Result{}, nil
	}
	if ad.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(ad, adFinalizer)
		return ctrl.Result{}, r.Update(ctx, ad)
	}
	if ad.Status.UyuniID != 0 {
		uc, err := r.Clients.ForOrganization(ctx, orgRef(ad.Spec.OrganizationRef), ad.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := uc.DeleteDistribution(ctx, ad.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(ad, adFinalizer)
	return ctrl.Result{}, r.Update(ctx, ad)
}

func (r *AutoinstallDistributionReconciler) resolveChannelLabel(ctx context.Context, ad *uyuniv1.AutoinstallDistribution) (label, wait string, err error) {
	var sc uyuniv1.SoftwareChannel
	if err := r.Get(ctx, types.NamespacedName{Namespace: ad.Namespace, Name: ad.Spec.ChannelRef.Name}, &sc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", fmt.Sprintf("SoftwareChannel %q not found", ad.Spec.ChannelRef.Name), nil
		}
		return "", "", err
	}
	if sc.Status.Label == "" {
		return "", fmt.Sprintf("SoftwareChannel %q not yet realized in Uyuni", ad.Spec.ChannelRef.Name), nil
	}
	return sc.Status.Label, "", nil
}

func (r *AutoinstallDistributionReconciler) fail(ctx context.Context, ad *uyuniv1.AutoinstallDistribution, reason string, err error) (ctrl.Result, error) {
	setReady(&ad.Status.Conditions, ad.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ad)
	return ctrl.Result{}, err
}

func distributionNeedsUpdate(current *uyuni.DistributionDetails, ad *uyuniv1.AutoinstallDistribution, channelLabel string) bool {
	// kickstart.tree.getDetails doesn't report the channel label or install
	// type, so channel changes are detected via the last-applied value in
	// status; install type is immutable.
	return current.BasePath != ad.Spec.BasePath ||
		current.KernelOptions != ad.Spec.KernelOptions ||
		current.PostKernelOptions != ad.Spec.PostKernelOptions ||
		ad.Status.ChannelLabel != channelLabel
}

func (r *AutoinstallDistributionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.AutoinstallDistribution{}).
		Watches(&uyuniv1.SoftwareChannel{},
			handler.EnqueueRequestsFromMapFunc(r.distributionsForChannel)).
		Complete(r)
}

func (r *AutoinstallDistributionReconciler) distributionsForChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.AutoinstallDistributionList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ad := range list.Items {
		if ad.Spec.ChannelRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ad.Namespace, Name: ad.Name},
			})
		}
	}
	return out
}
