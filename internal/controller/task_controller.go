package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

const maxRunHistory = 10

type TaskReconciler struct {
	client.Client
	Clients uyuni.ClientPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=tasks,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems;systemgroups,verbs=get;list;watch

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task uyuniv1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !task.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &task)
	}
	if ensureFinalizer(&task, taskFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &task)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(task.Spec.OrganizationRef), task.Namespace)
	if err != nil {
		return r.failTask(ctx, &task, "OrganizationError", err)
	}

	// Spec-shape validation lives in the webhook. Reconciler relies on the
	// dispatch in scheduleByKind to detect malformed spec at runtime
	// (e.g. webhook bypass): an unhandled kind there produces ScheduleFailed,
	// which is acceptable behavior.

	if err := r.refreshActiveRuns(ctx, uc, &task); err != nil {
		return ctrl.Result{}, err
	}

	trigger := r.decideTrigger(&task)
	if trigger != "" && !r.hasActiveRun(&task) {
		if err := r.startRun(ctx, uc, &task, trigger); err != nil {
			return r.failTask(ctx, &task, "ScheduleFailed", err)
		}
		// Update status FIRST so the run is durable, then strip the
		// annotation. Crash between schedule and status write means
		// duplicate run on retry, not lost intent — acceptable for tasks.
		if err := r.Status().Update(ctx, &task); err != nil {
			return ctrl.Result{}, err
		}
		if trigger == "annotation" && task.Annotations[uyuniv1.AnnRerun] == "true" {
			delete(task.Annotations, uyuniv1.AnnRerun)
			if err := r.Update(ctx, &task); err != nil {
				// Annotation cleanup failed; next reconcile re-triggers.
				// hasActiveRun guards against duplicate scheduling.
				return ctrl.Result{}, err
			}
		}
	}

	r.updatePhase(&task)
	task.Status.ObservedGeneration = task.Generation
	if r.isTerminalPhase(task.Status.Phase) {
		setReady(&task.Status.Conditions, task.Generation, metav1.ConditionTrue,
			"Reconciled", task.Status.Phase)
	} else {
		setReady(&task.Status.Conditions, task.Generation, metav1.ConditionFalse,
			"InProgress", task.Status.Phase)
	}
	if err := r.Status().Update(ctx, &task); err != nil {
		return ctrl.Result{}, err
	}

	if r.hasActiveRun(&task) {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if r.isTerminalPhase(task.Status.Phase) && task.Spec.TTLAfterFinished.Duration > 0 {
		if latest := r.latestRun(&task); latest != nil && latest.CompletedAt != nil {
			gcAt := latest.CompletedAt.Add(task.Spec.TTLAfterFinished.Duration)
			if r.Now().After(gcAt) {
				return ctrl.Result{}, r.Delete(ctx, &task)
			}
			return ctrl.Result{RequeueAfter: time.Until(gcAt)}, nil
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *TaskReconciler) decideTrigger(task *uyuniv1.Task) string {
	if task.Annotations[uyuniv1.AnnRerun] == "true" {
		return "annotation"
	}
	if len(task.Status.Runs) == 0 {
		return "initial"
	}
	if task.Status.ObservedGeneration != task.Generation {
		return "spec-change"
	}
	return ""
}

func (r *TaskReconciler) hasActiveRun(task *uyuniv1.Task) bool {
	for i := range task.Status.Runs {
		p := task.Status.Runs[i].Phase
		if p == "Pending" || p == "Running" {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) latestRun(task *uyuniv1.Task) *uyuniv1.TaskRun {
	if len(task.Status.Runs) == 0 {
		return nil
	}
	return &task.Status.Runs[len(task.Status.Runs)-1]
}

func (r *TaskReconciler) startRun(ctx context.Context, uc uyuni.API, task *uyuniv1.Task, trigger string) error {
	serverIDs, err := r.resolveTarget(ctx, task)
	if err != nil {
		return err
	}
	if len(serverIDs) == 0 {
		return fmt.Errorf("target resolved to zero systems")
	}

	earliest := time.Time{}
	if task.Spec.NotBefore != nil {
		earliest = task.Spec.NotBefore.Time
	}

	actionIDs, err := r.scheduleByKind(ctx, uc, task, serverIDs, earliest)
	if err != nil {
		return err
	}

	now := metav1.NewTime(r.Now())
	run := uyuniv1.TaskRun{
		Sequence:  len(task.Status.Runs) + 1,
		ActionIDs: actionIDs,
		Phase:     "Pending",
		StartedAt: &now,
		Trigger:   trigger,
	}
	task.Status.Runs = append(task.Status.Runs, run)
	task.Status.ResolvedSystemIDs = serverIDs
	if len(task.Status.Runs) > maxRunHistory {
		task.Status.Runs = task.Status.Runs[len(task.Status.Runs)-maxRunHistory:]
	}
	return nil
}

func (r *TaskReconciler) scheduleByKind(ctx context.Context, uc uyuni.API, task *uyuniv1.Task, serverIDs []int, earliest time.Time) ([]int, error) {
	switch {
	case task.Spec.Highstate != nil:
		id, err := uc.ScheduleHighstate(ctx, serverIDs, earliest, task.Spec.Highstate.Test)
		return []int{id}, err
	case task.Spec.RemoteCommand != nil:
		rc := task.Spec.RemoteCommand
		user := rc.User
		if user == "" {
			user = "root"
		}
		grp := rc.Group
		if grp == "" {
			grp = "root"
		}
		timeout := rc.TimeoutSeconds
		if timeout == 0 {
			timeout = 300
		}
		id, err := uc.ScheduleRemoteCommand(ctx, serverIDs, earliest, rc.Command, user, grp, timeout)
		return []int{id}, err
	case task.Spec.Reboot != nil:
		earliestReboot := earliest
		if task.Spec.Reboot.DelaySeconds > 0 {
			earliestReboot = r.Now().Add(time.Duration(task.Spec.Reboot.DelaySeconds) * time.Second)
		}
		return uc.ScheduleReboot(ctx, serverIDs, earliestReboot)
	case task.Spec.ApplyPatches != nil:
		return uc.ScheduleApplyPatches(ctx, serverIDs, earliest, task.Spec.ApplyPatches.IncludeAdvisories)
	case task.Spec.ApplyConfigChannels != nil:
		id, err := uc.ScheduleApplyConfigChannels(ctx, serverIDs, earliest)
		return []int{id}, err
	default:
		// Should be unreachable: webhook enforces exactly-one-of. Surfacing
		// as a hard error if we ever get here makes the webhook-bypass case
		// diagnosable.
		return nil, fmt.Errorf("no task kind specified (admission should have rejected; check webhook configuration)")
	}
}

func (r *TaskReconciler) refreshActiveRuns(ctx context.Context, uc uyuni.API, task *uyuniv1.Task) error {
	for i := range task.Status.Runs {
		run := &task.Status.Runs[i]
		if run.Phase != "Pending" && run.Phase != "Running" {
			continue
		}
		allResults := make([]uyuniv1.TaskRunResult, 0)
		anyRunning := false
		anyFailed := false
		anySucceeded := false
		for _, aid := range run.ActionIDs {
			results, err := uc.GetActionResults(ctx, aid)
			if err != nil {
				return err
			}
			for _, res := range results {
				allResults = append(allResults, uyuniv1.TaskRunResult{
					SystemID: res.ServerID, ActionID: res.ActionID,
					Status: res.Status, Output: res.Result, ExitCode: res.ExitCode,
				})
				switch res.Status {
				case "Running", "Pending":
					anyRunning = true
				case "Failed":
					anyFailed = true
				case "Succeeded":
					anySucceeded = true
				}
			}
		}
		run.Results = allResults
		switch {
		case anyRunning:
			run.Phase = "Running"
		case anyFailed && anySucceeded:
			run.Phase = "Mixed"
		case anyFailed:
			run.Phase = "Failed"
		case anySucceeded:
			run.Phase = "Succeeded"
		default:
			run.Phase = "Running"
		}
		if run.Phase == "Succeeded" || run.Phase == "Failed" || run.Phase == "Mixed" {
			now := metav1.NewTime(r.Now())
			run.CompletedAt = &now
		}
	}
	return nil
}

func (r *TaskReconciler) updatePhase(task *uyuniv1.Task) {
	if latest := r.latestRun(task); latest != nil {
		task.Status.Phase = latest.Phase
	} else {
		task.Status.Phase = ""
	}
}

func (r *TaskReconciler) isTerminalPhase(phase string) bool {
	return phase == "Succeeded" || phase == "Failed" || phase == "Mixed"
}

func (r *TaskReconciler) resolveTarget(ctx context.Context, task *uyuniv1.Task) ([]int, error) {
	t := task.Spec.Target
	switch {
	case t.SystemRef != nil:
		var sys uyuniv1.System
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: t.SystemRef.Name}, &sys); err != nil {
			return nil, fmt.Errorf("system ref: %w", err)
		}
		if sys.Status.UyuniServerID == 0 {
			return nil, fmt.Errorf("system %q not yet registered", sys.Name)
		}
		return []int{sys.Status.UyuniServerID}, nil
	case t.SystemGroupRef != nil:
		var sg uyuniv1.SystemGroup
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: t.SystemGroupRef.Name}, &sg); err != nil {
			return nil, fmt.Errorf("system group ref: %w", err)
		}
		ids := make([]int, 0, len(sg.Status.ResolvedMembers))
		for _, s := range sg.Status.ResolvedMembers {
			if id, ok := parseInt(s); ok {
				ids = append(ids, id)
			}
		}
		return ids, nil
	case len(t.ServerIDs) > 0:
		return t.ServerIDs, nil
	default:
		return nil, fmt.Errorf("target requires exactly one of systemRef, systemGroupRef, or serverIds (admission should have rejected)")
	}
}

func (r *TaskReconciler) handleDeletion(ctx context.Context, task *uyuniv1.Task) (ctrl.Result, error) {
	if !containsFinalizer(task, taskFinalizer) {
		return ctrl.Result{}, nil
	}
	if task.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(task, taskFinalizer)
		return ctrl.Result{}, r.Update(ctx, task)
	}
	uc, err := r.Clients.ForOrganization(ctx, orgRef(task.Spec.OrganizationRef), task.Namespace)
	if err == nil {
		for _, run := range task.Status.Runs {
			if run.Phase != "Pending" && run.Phase != "Running" {
				continue
			}
			for _, aid := range run.ActionIDs {
				_ = uc.CancelAction(ctx, aid)
			}
		}
	}
	removeFinalizer(task, taskFinalizer)
	return ctrl.Result{}, r.Update(ctx, task)
}

func (r *TaskReconciler) failTask(ctx context.Context, task *uyuniv1.Task, reason string, err error) (ctrl.Result, error) {
	setReady(&task.Status.Conditions, task.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, task)
	return ctrl.Result{}, err
}

func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.Task{}).
		Complete(r)
}
