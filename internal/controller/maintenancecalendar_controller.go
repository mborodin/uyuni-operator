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

type MaintenanceCalendarReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenancecalendars,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenancecalendars/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenancecalendars/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=maintenanceschedules,verbs=get;list;watch

func (r *MaintenanceCalendarReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mc uyuniv1.MaintenanceCalendar
	if err := r.Get(ctx, req.NamespacedName, &mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if migrateAnnotations(&mc) {
		return ctrl.Result{}, r.Update(ctx, &mc)
	}

	if !mc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &mc)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(mc.Spec.OrganizationRef), mc.Namespace)
	if err != nil {
		return r.fail(ctx, &mc, "ProviderError", err)
	}

	if ensureFinalizer(&mc, mcalFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &mc)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &mc, orgRef(mc.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// AnnRefreshNow one-shot trigger: detect, act, record, then strip —
	// tolerates a crash between the action and the strip (idempotent retry).
	if mc.Annotations[uyuniv1.AnnRefreshNow] == "true" {
		strategy := rescheduleStrategyOrDefault(mc.Spec.RescheduleStrategy)
		if err := uc.RefreshMaintenanceCalendar(ctx, mc.Spec.Label, strategy); err != nil {
			return r.fail(ctx, &mc, "UpdateFailed", err)
		}
		now := metav1.Now()
		mc.Status.LastRefreshedTime = &now
		if err := r.Status().Update(ctx, &mc); err != nil {
			return ctrl.Result{}, err
		}
		delete(mc.Annotations, uyuniv1.AnnRefreshNow)
		if err := r.Update(ctx, &mc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	existing, err := r.findCalendar(ctx, uc, mc.Spec.Label)
	if err != nil {
		return r.fail(ctx, &mc, "ProviderError", err)
	}
	if existing == nil {
		var created *uyuni.MaintenanceCalendarDetails
		if mc.Spec.ICal != "" {
			created, err = uc.CreateMaintenanceCalendar(ctx, mc.Spec.Label, mc.Spec.ICal)
		} else {
			created, err = uc.CreateMaintenanceCalendarWithURL(ctx, mc.Spec.Label, mc.Spec.URL)
		}
		if err != nil {
			return r.fail(ctx, &mc, "CreateFailed", err)
		}
		existing = created
	} else {
		// Drift compares against whichever source mode is in use — an
		// inline calendar's realized ical is compared to spec.ical; a
		// URL-backed calendar compares the source URL, not the fetched
		// content (that's a refresh-now concern, not spec drift).
		var drifted bool
		if mc.Spec.ICal != "" {
			drifted = existing.ICal != mc.Spec.ICal
		} else {
			drifted = existing.URL != mc.Spec.URL
		}
		if drifted {
			strategy := rescheduleStrategyOrDefault(mc.Spec.RescheduleStrategy)
			if err := uc.UpdateMaintenanceCalendar(ctx, mc.Spec.Label, mc.Spec.ICal, mc.Spec.URL, strategy); err != nil {
				return r.fail(ctx, &mc, "UpdateFailed", err)
			}
		}
	}

	mc.Status.UyuniID = existing.ID
	mc.Status.ObservedGeneration = mc.Generation
	setReady(&mc.Status.Conditions, mc.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &mc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *MaintenanceCalendarReconciler) handleDeletion(ctx context.Context, mc *uyuniv1.MaintenanceCalendar) (ctrl.Result, error) {
	if !containsFinalizer(mc, mcalFinalizer) {
		return ctrl.Result{}, nil
	}
	if mc.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(mc, mcalFinalizer)
		return ctrl.Result{}, r.Update(ctx, mc)
	}

	// InUse guard: block deletion while any MaintenanceSchedule still
	// references this calendar. Deleting it in Uyuni would drop the
	// schedule's windows out from under it.
	refs, err := r.referencingSchedules(ctx, mc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(refs) > 0 {
		setReady(&mc.Status.Conditions, mc.Generation, metav1.ConditionFalse,
			"InUse", fmt.Sprintf("maintenance calendar is referenced by MaintenanceSchedule(s): %v", refs))
		_ = r.Status().Update(ctx, mc)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(mc.Spec.OrganizationRef), mc.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := uc.DeleteMaintenanceCalendar(ctx, mc.Spec.Label, true); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(mc, mcalFinalizer)
	return ctrl.Result{}, r.Update(ctx, mc)
}

// findCalendar adopts an existing Uyuni calendar by label (probe via list,
// same idiom as CustomInfoKeyReconciler.findKey) rather than relying on a
// get-and-catch-not-found call whose error shape for this namespace isn't
// otherwise exercised in this codebase.
func (r *MaintenanceCalendarReconciler) findCalendar(ctx context.Context, uc uyuni.API, label string) (*uyuni.MaintenanceCalendarDetails, error) {
	labels, err := uc.ListMaintenanceCalendarLabels(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, l := range labels {
		if l == label {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	return uc.GetMaintenanceCalendarDetails(ctx, label)
}

// referencingSchedules returns the names of MaintenanceSchedule CRs in the
// same namespace that reference this calendar via spec.calendarRef.
func (r *MaintenanceCalendarReconciler) referencingSchedules(ctx context.Context, mc *uyuniv1.MaintenanceCalendar) ([]string, error) {
	var list uyuniv1.MaintenanceScheduleList
	if err := r.List(ctx, &list, client.InNamespace(mc.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, sched := range list.Items {
		if sched.Spec.CalendarRef != nil && sched.Spec.CalendarRef.Name == mc.Name {
			out = append(out, sched.Name)
		}
	}
	return out, nil
}

func (r *MaintenanceCalendarReconciler) fail(ctx context.Context, mc *uyuniv1.MaintenanceCalendar, reason string, err error) (ctrl.Result, error) {
	setReady(&mc.Status.Conditions, mc.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, mc)
	return ctrl.Result{}, err
}

func (r *MaintenanceCalendarReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.MaintenanceCalendar{}).
		Watches(&uyuniv1.MaintenanceSchedule{},
			handler.EnqueueRequestsFromMapFunc(r.calendarsForSchedule)).
		Complete(r)
}

// calendarsForSchedule re-triggers the referenced MaintenanceCalendar when a
// MaintenanceSchedule pointing at it changes (e.g. stops referencing it,
// unblocking a pending delete) — same idiom as
// CustomInfoKeyReconciler.keysForSystem.
func (r *MaintenanceCalendarReconciler) calendarsForSchedule(_ context.Context, obj client.Object) []reconcile.Request {
	sched, ok := obj.(*uyuniv1.MaintenanceSchedule)
	if !ok || sched.Spec.CalendarRef == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: sched.Namespace, Name: sched.Spec.CalendarRef.Name},
	}}
}
