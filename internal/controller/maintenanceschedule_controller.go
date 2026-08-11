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

type MaintenanceScheduleReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenanceschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenanceschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenanceschedules/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenancecalendars,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups,verbs=get;list;watch

func (r *MaintenanceScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ms uyuniv1.MaintenanceSchedule
	if err := r.Get(ctx, req.NamespacedName, &ms); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if migrateAnnotations(&ms) {
		return ctrl.Result{}, r.Update(ctx, &ms)
	}

	if !ms.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ms)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ms.Spec.OrganizationRef), ms.Namespace)
	if err != nil {
		return r.fail(ctx, &ms, "ProviderError", err)
	}

	if ensureFinalizer(&ms, mschedFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ms)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &ms, orgRef(ms.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	strategy := rescheduleStrategyOrDefault(ms.Spec.RescheduleStrategy)

	// Resolve calendarRef (optional — absent means unrestricted/24-7).
	calendarLabel, wait, err := r.resolveCalendar(ctx, &ms)
	if err != nil {
		return r.fail(ctx, &ms, "ResolveRefs", err)
	}
	if wait != "" {
		setReady(&ms.Status.Conditions, ms.Generation, metav1.ConditionFalse, "WaitingForCalendar", wait)
		_ = r.Status().Update(ctx, &ms)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Adopt-or-create the schedule itself.
	existing, err := r.findSchedule(ctx, uc, ms.Spec.Name)
	if err != nil {
		return r.fail(ctx, &ms, "ProviderError", err)
	}
	if existing == nil {
		created, err := uc.CreateMaintenanceSchedule(ctx, ms.Spec.Name, ms.Spec.Type, calendarLabel)
		if err != nil {
			return r.fail(ctx, &ms, "CreateFailed", err)
		}
		existing = created
	} else {
		existingCalLabel := ""
		if existing.Calendar != nil {
			existingCalLabel = existing.Calendar.Label
		}
		// Uyuni returns type lowercase ("multi") regardless of the case we
		// sent ("Multi"); compare case-insensitively so this doesn't drift-loop.
		if !strings.EqualFold(existing.Type, ms.Spec.Type) || existingCalLabel != calendarLabel {
			if err := uc.UpdateMaintenanceSchedule(ctx, ms.Spec.Name, ms.Spec.Type, calendarLabel, strategy); err != nil {
				return r.fail(ctx, &ms, "UpdateFailed", err)
			}
		}
	}

	// Resolve desired system assignment (direct refs + expanded group membership).
	desiredIDs, wait, err := r.resolveDesiredSystems(ctx, uc, &ms)
	if err != nil {
		return r.fail(ctx, &ms, "ResolveRefs", err)
	}
	if wait != "" {
		setReady(&ms.Status.Conditions, ms.Generation, metav1.ConditionFalse, "ReferenceUnavailable", wait)
		_ = r.Status().Update(ctx, &ms)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	currentIDs, err := uc.ListSystemsWithSchedule(ctx, ms.Spec.Name)
	if err != nil {
		return r.fail(ctx, &ms, "ProviderError", err)
	}
	toAdd, toRemove := diffIntSets(currentIDs, desiredIDs)
	if len(toAdd) > 0 {
		if err := uc.AssignScheduleToSystems(ctx, ms.Spec.Name, toAdd, strategy); err != nil {
			return r.fail(ctx, &ms, "ScheduleFailed", err)
		}
	}
	if len(toRemove) > 0 {
		if err := uc.RetractScheduleFromSystems(ctx, toRemove); err != nil {
			return r.fail(ctx, &ms, "ScheduleFailed", err)
		}
	}

	ms.Status.UyuniID = existing.ID
	ms.Status.ResolvedSystemIDs = desiredIDs
	ms.Status.SystemCount = len(desiredIDs)
	ms.Status.ObservedGeneration = ms.Generation
	setReady(&ms.Status.Conditions, ms.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &ms); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *MaintenanceScheduleReconciler) handleDeletion(ctx context.Context, ms *uyuniv1.MaintenanceSchedule) (ctrl.Result, error) {
	if !containsFinalizer(ms, mschedFinalizer) {
		return ctrl.Result{}, nil
	}
	if ms.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(ms, mschedFinalizer)
		return ctrl.Result{}, r.Update(ctx, ms)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ms.Spec.OrganizationRef), ms.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Retract everything currently assigned (read fresh from Uyuni, not
	// status — status is a cache) before deleting the schedule itself.
	currentIDs, err := uc.ListSystemsWithSchedule(ctx, ms.Spec.Name)
	if err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if len(currentIDs) > 0 {
		if err := uc.RetractScheduleFromSystems(ctx, currentIDs); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := uc.DeleteMaintenanceSchedule(ctx, ms.Spec.Name); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(ms, mschedFinalizer)
	return ctrl.Result{}, r.Update(ctx, ms)
}

// resolveCalendar returns the referenced MaintenanceCalendar's Uyuni label,
// or "" if spec.calendarRef is unset (unrestricted schedule). wait is
// non-empty if the reference isn't ready to use yet.
func (r *MaintenanceScheduleReconciler) resolveCalendar(ctx context.Context, ms *uyuniv1.MaintenanceSchedule) (label, wait string, err error) {
	if ms.Spec.CalendarRef == nil {
		return "", "", nil
	}
	var mc uyuniv1.MaintenanceCalendar
	if getErr := r.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: ms.Spec.CalendarRef.Name}, &mc); getErr != nil {
		if client.IgnoreNotFound(getErr) == nil {
			return "", fmt.Sprintf("MaintenanceCalendar %q not found", ms.Spec.CalendarRef.Name), nil
		}
		return "", "", getErr
	}
	if mc.Status.UyuniID == 0 {
		return "", fmt.Sprintf("MaintenanceCalendar %q not yet realized in Uyuni", ms.Spec.CalendarRef.Name), nil
	}
	return mc.Spec.Label, "", nil
}

// resolveDesiredSystems flattens spec.systemRefs and spec.systemGroupRefs
// into the set of Uyuni server IDs that should be assigned to this
// schedule. wait is non-empty if a reference isn't resolvable yet — the
// whole set is withheld rather than partially applied.
func (r *MaintenanceScheduleReconciler) resolveDesiredSystems(ctx context.Context, uc uyuni.API, ms *uyuniv1.MaintenanceSchedule) (ids []int, wait string, err error) {
	seen := map[int]bool{}

	for _, ref := range ms.Spec.SystemRefs {
		sys, findErr := findSystem(ctx, r.Client, ms.Namespace, ref.Name)
		if findErr != nil {
			return nil, "", findErr
		}
		if sys == nil {
			return nil, fmt.Sprintf("System %q not found", ref.Name), nil
		}
		if sys.Status.UyuniServerID == 0 {
			return nil, fmt.Sprintf("System %q not yet registered in Uyuni", ref.Name), nil
		}
		seen[sys.Status.UyuniServerID] = true
	}

	for _, ref := range ms.Spec.SystemGroupRefs {
		var sg uyuniv1.SystemGroup
		if getErr := r.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: ref.Name}, &sg); getErr != nil {
			if client.IgnoreNotFound(getErr) == nil {
				return nil, fmt.Sprintf("SystemGroup %q not found", ref.Name), nil
			}
			return nil, "", getErr
		}
		// Uyuni's maintenance API has no group-native assignment call —
		// expand membership to individual server IDs here. An empty group
		// (no members yet) is not an error, just contributes nothing.
		members, listErr := uc.ListSystemsInGroup(ctx, sg.Spec.Name)
		if listErr != nil {
			return nil, "", listErr
		}
		for _, id := range members {
			seen[id] = true
		}
	}

	out := make([]int, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, "", nil
}

// findSchedule adopts an existing Uyuni schedule by name (probe via list,
// same idiom as CustomInfoKeyReconciler.findKey).
func (r *MaintenanceScheduleReconciler) findSchedule(ctx context.Context, uc uyuni.API, name string) (*uyuni.MaintenanceScheduleDetails, error) {
	names, err := uc.ListMaintenanceScheduleNames(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	return uc.GetMaintenanceScheduleDetails(ctx, name)
}

func (r *MaintenanceScheduleReconciler) fail(ctx context.Context, ms *uyuniv1.MaintenanceSchedule, reason string, err error) (ctrl.Result, error) {
	setReady(&ms.Status.Conditions, ms.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ms)
	return ctrl.Result{}, err
}

func (r *MaintenanceScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.MaintenanceSchedule{}).
		Watches(&uyuniv1.System{},
			handler.EnqueueRequestsFromMapFunc(r.schedulesForSystem)).
		Watches(&uyuniv1.SystemGroup{},
			handler.EnqueueRequestsFromMapFunc(r.schedulesForSystemGroup)).
		Watches(&uyuniv1.MaintenanceCalendar{},
			handler.EnqueueRequestsFromMapFunc(r.schedulesForCalendar)).
		Complete(r)
}

// schedulesForSystem re-triggers MaintenanceSchedules that directly
// reference a changed System (e.g. registration just completed).
func (r *MaintenanceScheduleReconciler) schedulesForSystem(ctx context.Context, obj client.Object) []reconcile.Request {
	sys, ok := obj.(*uyuniv1.System)
	if !ok {
		return nil
	}
	var list uyuniv1.MaintenanceScheduleList
	if err := r.List(ctx, &list, client.InNamespace(sys.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ms := range list.Items {
		for _, ref := range ms.Spec.SystemRefs {
			if systemRefMatches(ref, sys) {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name},
				})
				break
			}
		}
	}
	return out
}

// schedulesForSystemGroup re-triggers MaintenanceSchedules that reference a
// changed SystemGroup (membership may have drifted).
func (r *MaintenanceScheduleReconciler) schedulesForSystemGroup(ctx context.Context, obj client.Object) []reconcile.Request {
	sg, ok := obj.(*uyuniv1.SystemGroup)
	if !ok {
		return nil
	}
	var list uyuniv1.MaintenanceScheduleList
	if err := r.List(ctx, &list, client.InNamespace(sg.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ms := range list.Items {
		for _, ref := range ms.Spec.SystemGroupRefs {
			if ref.Name == sg.Name {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name},
				})
				break
			}
		}
	}
	return out
}

// schedulesForCalendar re-triggers MaintenanceSchedules waiting on a
// calendar that just became Ready.
func (r *MaintenanceScheduleReconciler) schedulesForCalendar(ctx context.Context, obj client.Object) []reconcile.Request {
	mc, ok := obj.(*uyuniv1.MaintenanceCalendar)
	if !ok {
		return nil
	}
	var list uyuniv1.MaintenanceScheduleList
	if err := r.List(ctx, &list, client.InNamespace(mc.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ms := range list.Items {
		if ms.Spec.CalendarRef != nil && ms.Spec.CalendarRef.Name == mc.Name {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name},
			})
		}
	}
	return out
}
