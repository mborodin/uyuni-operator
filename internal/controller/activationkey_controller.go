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

type ActivationKeyReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels;systemgroups;configurationchannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects/finalizers,verbs=update

func (r *ActivationKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ak uyuniv1.ActivationKey
	if err := r.Get(ctx, req.NamespacedName, &ak); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ak.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ak)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ak.Spec.OrganizationRef), ak.Namespace)
	if err != nil {
		return r.fail(ctx, &ak, "OrganizationError", err)
	}

	if ensureFinalizer(&ak, akFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ak)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &ak, orgRef(ak.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve channel + group refs. Webhook validates mutual exclusion at
	// admission; the resolver still flags it defensively if admission was bypassed.
	res, err := resolveChannelRefs(ctx, r.Client, ak.Namespace, channelRefs{
		BaseChannelRef:    ak.Spec.BaseChannelRef,
		ChildChannelRefs:  ak.Spec.ChildChannelRefs,
		BaseChannelFrom:   ak.Spec.BaseChannelFrom,
		ChildChannelsFrom: ak.Spec.ChildChannelsFrom,
	})
	if err != nil {
		return r.fail(ctx, &ak, "ResolveRefs", err)
	}
	if res.HardError != "" {
		// Reference disappeared/inconsistent at reconcile time. Distinct from
		// admission rejection: this is "referenced thing is currently
		// unavailable", not "spec is invalid".
		setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse,
			"ReferenceUnavailable", res.HardError)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, &ak)
	}
	if res.WaitReason != "" {
		setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse,
			res.WaitReason, res.WaitDetail)
		if err := r.Status().Update(ctx, &ak); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Maintain owner refs to ContentProjects we reference. This drives the
	// Kubernetes-native cascade-on-delete behavior.
	if err := reconcileProjectOwnership(ctx, r.Client, &ak, projectOwnersFromActivationKey(&ak)); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve system groups (separate path because they need the UyuniID
	// from each group's status, not a channel label).
	groupIDs, groupWait, err := r.resolveSystemGroups(ctx, &ak)
	if err != nil {
		return r.fail(ctx, &ak, "ResolveSystemGroups", err)
	}
	if groupWait != "" {
		setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse,
			"WaitingForSystemGroup", groupWait)
		if err := r.Status().Update(ctx, &ak); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	configChannelLabels, configWait, err := r.resolveConfigChannels(ctx, &ak)
	if err != nil {
		return r.fail(ctx, &ak, "ResolveConfigChannels", err)
	}
	if configWait != "" {
		setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse,
			"WaitingForConfigChannel", configWait)
		if err := r.Status().Update(ctx, &ak); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Find or create the activation key. Adoption logic (probe-by-predicted-name)
	// is in fetchExisting.
	current, err := r.fetchExisting(ctx, uc, &ak)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current == nil {
		key, err := uc.CreateActivationKey(ctx, uyuni.ActivationKeyDetails{
			Key:              ak.Spec.Key,
			Description:      ak.Spec.Description,
			BaseChannelLabel: res.BaseChannelLabel,
			UsageLimit:       ak.Spec.UsageLimit,
			Entitlements:     ak.Spec.Entitlements,
			UniversalDefault: ak.Spec.UniversalDefault,
		})
		if err != nil {
			return r.fail(ctx, &ak, "CreateFailed", err)
		}
		ak.Status.UyuniKey = key
		if err := r.Status().Update(ctx, &ak); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch on next reconcile to settle into the steady-state path.
		return ctrl.Result{Requeue: true}, nil
	}

	// Drift reconciliation. Children, config channels, and groups each have
	// their own add/remove APIs in Uyuni, so we diff and apply per-list.
	if needsDetailsUpdate(current, &ak, res.BaseChannelLabel) {
		if err := uc.SetActivationKeyDetails(ctx, ak.Status.UyuniKey, uyuni.ActivationKeyDetails{
			Description:      ak.Spec.Description,
			BaseChannelLabel: res.BaseChannelLabel,
			UsageLimit:       ak.Spec.UsageLimit,
			UniversalDefault: ak.Spec.UniversalDefault,
			ContactMethod:    ak.Spec.ContactMethod,
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	if addCh, rmCh := diffStringSets(current.ChildChannels, res.ChildChannelLabels); len(addCh)+len(rmCh) > 0 {
		if err := uc.AddChildChannels(ctx, ak.Status.UyuniKey, addCh); err != nil {
			return ctrl.Result{}, err
		}
		if err := uc.RemoveChildChannels(ctx, ak.Status.UyuniKey, rmCh); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !stringSlicesEqual(current.ConfigChannels, configChannelLabels) {
		if err := uc.SetActivationKeyConfigChannels(ctx, ak.Status.UyuniKey, configChannelLabels); err != nil {
			return ctrl.Result{}, err
		}
	}

	if addGrp, rmGrp := diffIntSets(current.ServerGroupIDs, groupIDs); len(addGrp)+len(rmGrp) > 0 {
		if err := uc.SetActivationKeyGroups(ctx, ak.Status.UyuniKey, groupIDs); err != nil {
			return ctrl.Result{}, err
		}
	}

	ak.Status.ObservedGeneration = ak.Generation
	setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &ak); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ActivationKeyReconciler) handleDeletion(ctx context.Context, ak *uyuniv1.ActivationKey) (ctrl.Result, error) {
	if !containsFinalizer(ak, akFinalizer) {
		return ctrl.Result{}, nil
	}
	if ak.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(ak, akFinalizer)
		return ctrl.Result{}, r.Update(ctx, ak)
	}
	if ak.Status.UyuniKey != "" {
		uc, err := r.Clients.ForOrganization(ctx, orgRef(ak.Spec.OrganizationRef), ak.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := uc.DeleteActivationKey(ctx, ak.Status.UyuniKey); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(ak, akFinalizer)
	return ctrl.Result{}, r.Update(ctx, ak)
}

func (r *ActivationKeyReconciler) fetchExisting(ctx context.Context, uc uyuni.API, ak *uyuniv1.ActivationKey) (*uyuni.ActivationKeyDetails, error) {
	if ak.Status.UyuniKey != "" {
		d, err := uc.GetActivationKey(ctx, ak.Status.UyuniKey)
		if uyuni.IsNotFound(err) {
			ak.Status.UyuniKey = ""
			return nil, nil
		}
		return d, err
	}
	// Adoption probe: predict the org-prefixed key using the org's actual Uyuni ID.
	orgID := r.resolveOrgID(ctx, ak)
	predicted := fmt.Sprintf("%d-%s", orgID, ak.Spec.Key)
	d, err := uc.GetActivationKey(ctx, predicted)
	if uyuni.IsNotFound(err) || err != nil {
		// Treat any error as not-found: the key may not exist yet, or it may be in a different org.
		return nil, nil
	}
	ak.Status.UyuniKey = predicted
	return d, nil
}

func (r *ActivationKeyReconciler) resolveOrgID(ctx context.Context, ak *uyuniv1.ActivationKey) int {
	if ak.Spec.OrganizationRef == nil {
		return 1
	}
	var org uyuniv1.Organization
	if err := r.Get(ctx, types.NamespacedName{Namespace: ak.Namespace, Name: ak.Spec.OrganizationRef.Name}, &org); err != nil {
		return 1
	}
	if org.Status.UyuniOrgID == 0 {
		return 1
	}
	return org.Status.UyuniOrgID
}

func (r *ActivationKeyReconciler) resolveSystemGroups(ctx context.Context, ak *uyuniv1.ActivationKey) (ids []int, wait string, err error) {
	for _, ref := range ak.Spec.SystemGroupRefs {
		var sg uyuniv1.SystemGroup
		if err := r.Get(ctx, types.NamespacedName{Namespace: ak.Namespace, Name: ref.Name}, &sg); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("SystemGroup %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if sg.Status.UyuniID == 0 {
			return nil, fmt.Sprintf("SystemGroup %q not yet realized", ref.Name), nil
		}
		ids = append(ids, sg.Status.UyuniID)
	}
	return ids, "", nil
}

func (r *ActivationKeyReconciler) resolveConfigChannels(ctx context.Context, ak *uyuniv1.ActivationKey) (labels []string, wait string, err error) {
	for _, ref := range ak.Spec.ConfigChannelRefs {
		var cc uyuniv1.ConfigurationChannel
		if err := r.Get(ctx, types.NamespacedName{Namespace: ak.Namespace, Name: ref.Name}, &cc); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("ConfigurationChannel %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if cc.Status.UyuniID == 0 {
			return nil, fmt.Sprintf("ConfigurationChannel %q not yet realized", ref.Name), nil
		}
		labels = append(labels, cc.Spec.ID)
	}
	return labels, "", nil
}

func (r *ActivationKeyReconciler) fail(ctx context.Context, ak *uyuniv1.ActivationKey, reason string, err error) (ctrl.Result, error) {
	setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ak)
	return ctrl.Result{}, err
}

func needsDetailsUpdate(current *uyuni.ActivationKeyDetails, ak *uyuniv1.ActivationKey, desiredBaseLabel string) bool {
	return current.Description != ak.Spec.Description ||
		current.BaseChannelLabel != desiredBaseLabel ||
		current.UsageLimit != ak.Spec.UsageLimit ||
		current.UniversalDefault != ak.Spec.UniversalDefault ||
		current.ContactMethod != ak.Spec.ContactMethod
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := map[string]bool{}
	for _, s := range a {
		as[s] = true
	}
	for _, s := range b {
		if !as[s] {
			return false
		}
	}
	return true
}

func (r *ActivationKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ActivationKey{}).
		Watches(&uyuniv1.ContentProject{},
			handler.EnqueueRequestsFromMapFunc(r.activationKeysForProject)).
		Watches(&uyuniv1.SoftwareChannel{},
			handler.EnqueueRequestsFromMapFunc(r.activationKeysForChannel)).
		Watches(&uyuniv1.SystemGroup{},
			handler.EnqueueRequestsFromMapFunc(r.activationKeysForSystemGroup)).
		Complete(r)
}

func (r *ActivationKeyReconciler) activationKeysForSystemGroup(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ActivationKeyList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ak := range list.Items {
		for _, ref := range ak.Spec.SystemGroupRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ak.Namespace, Name: ak.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *ActivationKeyReconciler) activationKeysForProject(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ActivationKeyList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ak := range list.Items {
		if refsActivationKeyProject(&ak, obj.GetName()) {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ak.Namespace, Name: ak.Name},
			})
		}
	}
	return out
}

func (r *ActivationKeyReconciler) activationKeysForChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ActivationKeyList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ak := range list.Items {
		if ak.Spec.BaseChannelRef != nil && ak.Spec.BaseChannelRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ak.Namespace, Name: ak.Name},
			})
			continue
		}
		for _, c := range ak.Spec.ChildChannelRefs {
			if c.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ak.Namespace, Name: ak.Name},
				})
				break
			}
		}
	}
	return out
}
