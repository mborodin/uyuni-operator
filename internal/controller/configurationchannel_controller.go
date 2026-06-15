package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type ConfigurationChannelReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels/finalizers,verbs=update

func (r *ConfigurationChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cc uyuniv1.ConfigurationChannel
	if err := r.Get(ctx, req.NamespacedName, &cc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var uc uyuni.API
	var err error
	if cc.Spec.OrganizationRef != "" {
		uc, err = r.Clients.ForOrganization(ctx, cc.Spec.OrganizationRef, cc.Namespace)
	} else {
		var clusterRef *uyuni.LocalObjectRef
		if cc.Spec.Cluster != "" {
			clusterRef = &uyuni.LocalObjectRef{Name: cc.Spec.Cluster}
		}
		uc, err = r.Clients.For(ctx, clusterRef, cc.Namespace)
	}
	if err != nil {
		return r.fail(ctx, &cc, "ProviderError", err)
	}

	if !cc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &cc)
	}
	if ensureFinalizer(&cc, confChanFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cc)
	}

	current, err := uc.GetConfigChannel(ctx, cc.Spec.ID)
	if uyuni.IsNotFound(err) {
		created, createErr := uc.CreateConfigChannel(ctx, cc.Spec.ID, cc.Spec.Name, cc.Spec.Description, cc.Spec.Type)
		if createErr != nil {
			return r.fail(ctx, &cc, "CreateFailed", createErr)
		}
		cc.Status.UyuniID = created.ID
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		cc.Status.UyuniID = current.ID

		if current.Name != cc.Spec.Name || current.Description != cc.Spec.Description {
			if err := uc.UpdateConfigChannel(ctx, cc.Spec.ID, cc.Spec.Name, cc.Spec.Description); err != nil {
				return ctrl.Result{}, err
			}
		}

		if current.Type != cc.Spec.Type {
			setDrift(&cc.Status.Conditions, cc.Generation, true, "ImmutableFieldDrift",
				fmt.Sprintf("type in Uyuni (%s) differs from spec (%s); recreate to reconcile",
					current.Type, cc.Spec.Type))
		} else {
			setDrift(&cc.Status.Conditions, cc.Generation, false, "InSync", "")
		}
	}

	cc.Status.ObservedGeneration = cc.Generation
	setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &cc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ConfigurationChannelReconciler) handleDeletion(ctx context.Context, uc uyuni.API, cc *uyuniv1.ConfigurationChannel) (ctrl.Result, error) {
	if !containsFinalizer(cc, confChanFinalizer) {
		return ctrl.Result{}, nil
	}
	if cc.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(cc, confChanFinalizer)
		return ctrl.Result{}, r.Update(ctx, cc)
	}
	if err := uc.DeleteConfigChannel(ctx, cc.Spec.ID); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(cc, confChanFinalizer)
	return ctrl.Result{}, r.Update(ctx, cc)
}

func (r *ConfigurationChannelReconciler) fail(ctx context.Context, cc *uyuniv1.ConfigurationChannel, reason string, err error) (ctrl.Result, error) {
	setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, cc)
	return ctrl.Result{}, err
}

func (r *ConfigurationChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ConfigurationChannel{}).
		Complete(r)
}
