package controller

import (
	"context"
	"fmt"
	"strings"
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

type SystemGroupReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels,verbs=get;list;watch

func (r *SystemGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sg uyuniv1.SystemGroup
	if err := r.Get(ctx, req.NamespacedName, &sg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sg.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &sg)
	}

	// Resolve Uyuni client using organization context (organization takes precedence for API scope)
	// The cluster field specifies which provider manages this group (for future multi-cluster org support)
	uc, err := r.Clients.ForOrganization(ctx, orgRef(sg.Spec.OrganizationRef), sg.Namespace)
	if err != nil {
		return r.fail(ctx, &sg, "OrganizationError", err)
	}

	if ensureFinalizer(&sg, sgFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &sg)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &sg, orgRef(sg.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Create or adopt the group in Uyuni.
	current, err := uc.GetSystemGroup(ctx, sg.Spec.Name)
	if uyuni.IsNotFound(err) || (err != nil && strings.Contains(strings.ToLower(err.Error()), "unable to locate")) {
		created, createErr := uc.CreateSystemGroup(ctx, sg.Spec.Name, sg.Spec.Description)
		if createErr != nil {
			// Race or pre-existing group: adopt it rather than failing.
			if strings.Contains(strings.ToLower(createErr.Error()), "already exists") ||
				strings.Contains(strings.ToLower(createErr.Error()), "duplicate") {
				existing, getErr := uc.GetSystemGroup(ctx, sg.Spec.Name)
				if getErr != nil {
					return r.fail(ctx, &sg, "CreateFailed", createErr)
				}
				sg.Status.UyuniID = existing.ID
			} else {
				return r.fail(ctx, &sg, "CreateFailed", createErr)
			}
		} else {
			sg.Status.UyuniID = created.ID
		}
	} else if err != nil {
		return r.fail(ctx, &sg, "GetFailed", err)
	} else {
		sg.Status.UyuniID = current.ID
		if current.Description != sg.Spec.Description {
			if err := uc.UpdateSystemGroupDescription(ctx, sg.Spec.Name, sg.Spec.Description); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Resolve desired members from memberRefs and staticMinionIds.
	desiredIDs, minionIDs, waitReason, err := r.resolveMembers(ctx, uc, &sg)
	if err != nil {
		return r.fail(ctx, &sg, "ResolveMembersFailed", err)
	}
	if waitReason != "" {
		setReady(&sg.Status.Conditions, sg.Generation, metav1.ConditionFalse, "WaitingForMember", waitReason)
		_ = r.Status().Update(ctx, &sg)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Sync group membership.
	currentIDs, err := uc.ListSystemsInGroup(ctx, sg.Spec.Name)
	if err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if add, rm := diffIntSets(currentIDs, desiredIDs); len(add)+len(rm) > 0 {
		if len(add) > 0 {
			if err := uc.AddSystemsToGroup(ctx, sg.Spec.Name, add); err != nil {
				return ctrl.Result{}, err
			}
		}
		if len(rm) > 0 {
			if err := uc.RemoveSystemsFromGroup(ctx, sg.Spec.Name, rm); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Resolve and sync config channel subscriptions.
	desiredCCs, ccWait, err := r.resolveConfigChannels(ctx, &sg)
	if err != nil {
		return r.fail(ctx, &sg, "ResolveConfigChannelsFailed", err)
	}
	if ccWait != "" {
		setReady(&sg.Status.Conditions, sg.Generation, metav1.ConditionFalse, "WaitingForConfigChannel", ccWait)
		_ = r.Status().Update(ctx, &sg)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	addCCs, rmCCs := diffStringSets(sg.Status.ActiveConfigChannelLabels, desiredCCs)
	for _, label := range addCCs {
		if err := uc.SubscribeGroupToConfigChannel(ctx, sg.Spec.Name, label); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, label := range rmCCs {
		if err := uc.UnsubscribeGroupFromConfigChannel(ctx, sg.Spec.Name, label); err != nil {
			return ctrl.Result{}, err
		}
	}

	sg.Status.MemberCount = len(desiredIDs)
	sg.Status.ResolvedMembers = minionIDs
	sg.Status.ActiveConfigChannelLabels = desiredCCs
	sg.Status.ObservedGeneration = sg.Generation
	setReady(&sg.Status.Conditions, sg.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &sg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *SystemGroupReconciler) handleDeletion(ctx context.Context, sg *uyuniv1.SystemGroup) (ctrl.Result, error) {
	if !containsFinalizer(sg, sgFinalizer) {
		return ctrl.Result{}, nil
	}
	if sg.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(sg, sgFinalizer)
		return ctrl.Result{}, r.Update(ctx, sg)
	}
	if sg.Status.UyuniID != 0 {
		uc, err := r.Clients.ForOrganization(ctx, orgRef(sg.Spec.OrganizationRef), sg.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := uc.DeleteSystemGroup(ctx, sg.Spec.Name); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(sg, sgFinalizer)
	return ctrl.Result{}, r.Update(ctx, sg)
}

func (r *SystemGroupReconciler) resolveMembers(ctx context.Context, uc uyuni.API, sg *uyuniv1.SystemGroup) (ids []int, minionIDs []string, wait string, err error) {
	for _, ref := range sg.Spec.MemberRefs {
		var sys uyuniv1.System
		if err := r.Get(ctx, types.NamespacedName{Namespace: sg.Namespace, Name: ref.Name}, &sys); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, nil, fmt.Sprintf("System %q not found", ref.Name), nil
			}
			return nil, nil, "", err
		}
		if sys.Status.UyuniServerID == 0 {
			return nil, nil, fmt.Sprintf("System %q not yet registered in Uyuni", ref.Name), nil
		}
		ids = append(ids, sys.Status.UyuniServerID)
		minionIDs = append(minionIDs, sys.Spec.MinionID)
	}

	for _, minionID := range sg.Spec.StaticMinionIDs {
		details, findErr := uc.FindSystemByMinionID(ctx, minionID)
		if uyuni.IsNotFound(findErr) {
			return nil, nil, fmt.Sprintf("system with minionId %q not yet registered in Uyuni", minionID), nil
		}
		if findErr != nil {
			return nil, nil, "", findErr
		}
		ids = append(ids, details.ID)
		minionIDs = append(minionIDs, minionID)
	}

	return ids, minionIDs, "", nil
}

func (r *SystemGroupReconciler) resolveConfigChannels(ctx context.Context, sg *uyuniv1.SystemGroup) (labels []string, wait string, err error) {
	for _, ref := range sg.Spec.ConfigChannelRefs {
		cc, err := r.findConfigChannel(ctx, sg.Namespace, ref.Name)
		if err != nil {
			return nil, "", err
		}
		if cc == nil {
			return nil, fmt.Sprintf("ConfigurationChannel %q not found", ref.Name), nil
		}
		if cc.Status.UyuniID == 0 {
			return nil, fmt.Sprintf("ConfigurationChannel %q not yet realized", ref.Name), nil
		}
		labels = append(labels, cc.Spec.ID)
	}
	return labels, "", nil
}

// findConfigChannel looks up a ConfigurationChannel by CR name first, then by spec.id.
// This allows configChannelRefs to reference channels either by their Kubernetes CR name
// or by their Uyuni channel ID, which is useful for cross-XR references.
func (r *SystemGroupReconciler) findConfigChannel(ctx context.Context, namespace, nameOrID string) (*uyuniv1.ConfigurationChannel, error) {
	var cc uyuniv1.ConfigurationChannel
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: nameOrID}, &cc); err == nil {
		return &cc, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	// Not found by CR name — fall back to matching by spec.id.
	var list uyuniv1.ConfigurationChannelList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.ID == nameOrID {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

func (r *SystemGroupReconciler) fail(ctx context.Context, sg *uyuniv1.SystemGroup, reason string, err error) (ctrl.Result, error) {
	setReady(&sg.Status.Conditions, sg.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, sg)
	return ctrl.Result{}, err
}

func (r *SystemGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.SystemGroup{}).
		Watches(&uyuniv1.System{},
			handler.EnqueueRequestsFromMapFunc(r.groupsForSystem)).
		Watches(&uyuniv1.ConfigurationChannel{},
			handler.EnqueueRequestsFromMapFunc(r.groupsForConfigChannel)).
		Complete(r)
}

func (r *SystemGroupReconciler) groupsForSystem(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SystemGroupList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sg := range list.Items {
		for _, ref := range sg.Spec.MemberRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sg.Namespace, Name: sg.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *SystemGroupReconciler) groupsForConfigChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	crName := obj.GetName()
	var specID string
	if cc, ok := obj.(*uyuniv1.ConfigurationChannel); ok {
		specID = cc.Spec.ID
	}

	var list uyuniv1.SystemGroupList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sg := range list.Items {
		for _, ref := range sg.Spec.ConfigChannelRefs {
			if ref.Name == crName || (specID != "" && ref.Name == specID) {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sg.Namespace, Name: sg.Name},
				})
				break
			}
		}
	}
	return out
}
