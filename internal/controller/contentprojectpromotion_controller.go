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

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type ContentProjectPromotionReconciler struct {
	client.Client
	Clients uyuni.ClientPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojectpromotions,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojectpromotions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=get;list;watch

func (r *ContentProjectPromotionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p uyuniv1.ContentProjectPromotion
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal-phase GC: delete the resource after TTL expires past CompletedAt.
	if isTerminal(p.Status.Phase) {
		if p.Spec.TTLAfterFinished.Duration > 0 && p.Status.CompletedAt != nil {
			gcAt := p.Status.CompletedAt.Add(p.Spec.TTLAfterFinished.Duration)
			if r.Now().After(gcAt) {
				return ctrl.Result{}, r.Delete(ctx, &p)
			}
			return ctrl.Result{RequeueAfter: time.Until(gcAt)}, nil
		}
		return ctrl.Result{}, nil
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(p.Spec.OrganizationRef), p.Namespace)
	if err != nil {
		return r.fail(ctx, &p, err)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &p, orgRef(p.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	var cp uyuniv1.ContentProject
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.ProjectRef.Name}, &cp); err != nil {
		return r.fail(ctx, &p, fmt.Errorf("project ref: %w", err))
	}

	// Webhook validated structural promotion pair at admission. The
	// reconciler only checks runtime state: env still exists, project is
	// in a buildable state, version gate (if any) matches.
	sourceState := findEnvState(cp.Status.EnvironmentStates, p.Spec.FromEnvironment)
	if sourceState == nil {
		return r.fail(ctx, &p, fmt.Errorf(
			"environment %q no longer exists in ContentProject %q (project was modified after promotion was created)",
			p.Spec.FromEnvironment, cp.Name))
	}

	if p.Spec.NotBefore != nil && r.Now().Before(p.Spec.NotBefore.Time) {
		return r.pending(ctx, &p, "NotBefore",
			fmt.Sprintf("scheduled for %s", p.Spec.NotBefore.Time),
			time.Until(p.Spec.NotBefore.Time))
	}

	if p.Spec.RequireSourceVersion > 0 {
		if sourceState.BuiltVersion == 0 {
			return r.pending(ctx, &p, "SourceNotBuilt",
				fmt.Sprintf("source env %q has no build yet", p.Spec.FromEnvironment),
				30*time.Second)
		}
		if sourceState.BuiltVersion != p.Spec.RequireSourceVersion {
			return r.fail(ctx, &p, fmt.Errorf(
				"source version mismatch: requireSourceVersion=%d but env is at v%d",
				p.Spec.RequireSourceVersion, sourceState.BuiltVersion))
		}
	}

	if cp.Status.BuildStatus == "Building" {
		return r.pending(ctx, &p, "ProjectBuilding",
			"waiting for in-flight build of "+cp.Spec.Label,
			30*time.Second)
	}

	// Execute promotion. Uyuni's API promotes the named env to its successor,
	// so we pass FromEnvironment.
	now := metav1.NewTime(r.Now())
	if p.Status.StartedAt == nil {
		p.Status.StartedAt = &now
		p.Status.Phase = "Running"
		p.Status.PromotedVersion = sourceState.BuiltVersion
		if err := r.Status().Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}

	// CONFIRMED LIVE (VerificationTest promote action testing) - a real,
	// serious bug, found the hard way: this call used to run
	// unconditionally on every reconcile pass, with nothing recording
	// whether it had already been made. Watched it call Uyuni's
	// promoteProject API every ~30s for 13+ minutes straight - EVERY
	// single call rejected with "Build/Promote already in progress" -
	// while the target environment visibly cycled between "Cloning
	// channels" and "Waiting for repositories data to be generated" in
	// the Uyuni WebUI and never once reached Built. Strong evidence the
	// repeated calls were themselves interrupting/restarting Uyuni's own
	// in-flight promotion, not harmlessly no-op'ing against it -
	// PromoteProject is not safe to call again once it has already
	// succeeded for this promotion. Fixed: call it at most once, guarded
	// by status.promotionRequested, only set true after a successful
	// call. Every reconcile after that just polls
	// ListProjectEnvironments below - never calls PromoteProject again
	// for this CR.
	if !p.Status.PromotionRequested {
		if err := uc.PromoteProject(ctx, cp.Spec.Label, p.Spec.FromEnvironment); err != nil {
			// The BuildStatus=="Building" guard above is a best-effort
			// check, not authoritative - it can read "Idle" (stale,
			// lagging the real Uyuni state, same class of status-latency
			// this repo has hit elsewhere) right before Uyuni itself
			// rejects this first PromoteProject call with a transient
			// lock conflict (e.g. a build from something else still
			// finishing). Uyuni's CLM subsystem serializes build/promote
			// per project, so this class of conflict on the FIRST call
			// is expected to clear on its own - retry (still guarded by
			// promotionRequested staying false, so this branch is the
			// only place PromoteProject gets called) instead of failing
			// terminally. Any other error (bad project/env, permissions,
			// etc.) still fails immediately below.
			if isTransientCLMConflict(err) {
				setReady(&p.Status.Conditions, p.Generation, metav1.ConditionFalse,
					"CLMBusy", "Uyuni reported a build/promote already in progress for this project - retrying: "+err.Error())
				if uerr := r.Status().Update(ctx, &p); uerr != nil {
					return ctrl.Result{}, uerr
				}
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return r.fail(ctx, &p, err)
		}
		p.Status.PromotionRequested = true
		if err := r.Status().Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Promotion is async in Uyuni; poll the target env's status.
	envs, err := uc.ListProjectEnvironments(ctx, cp.Spec.Label)
	if err != nil {
		return ctrl.Result{}, err
	}
	var target *uyuni.ProjectEnvironmentInfo
	for i := range envs {
		if envs[i].Label == p.Spec.ToEnvironment {
			target = &envs[i]
			break
		}
	}
	if target == nil {
		return r.fail(ctx, &p, fmt.Errorf("target environment %q disappeared", p.Spec.ToEnvironment))
	}

	// CONFIRMED LIVE (VerificationTest promote action testing, via a
	// temporary diagnostic log that has since been removed): Uyuni
	// returns environment status in lowercase ("built", "building",
	// etc.), not the uppercase this switch was written to expect
	// ("BUILT", "BUILDING"). Go string comparison is case-sensitive, so
	// every real status value fell through to default and just kept
	// requeuing forever - a promotion that had genuinely finished in
	// Uyuni (confirmed in the WebUI) stayed at phase=Running in this CR
	// indefinitely, with no error, because nothing ever matched. Fixed
	// by uppercasing before the switch, so the case values here stay
	// the documented/readable constants (see the Status field's comment
	// in internal/uyuni/types.go).
	switch strings.ToUpper(target.Status) {
	case "BUILDING", "GENERATING_REPODATA", "NEW":
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case "FAILED":
		return r.fail(ctx, &p, fmt.Errorf("target environment build failed in Uyuni"))
	case "BUILT":
		done := metav1.NewTime(r.Now())
		p.Status.CompletedAt = &done
		p.Status.Phase = "Succeeded"
		setReady(&p.Status.Conditions, p.Generation, metav1.ConditionTrue, "Promoted", "")
		if err := r.Status().Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
		if p.Spec.TTLAfterFinished.Duration > 0 {
			return ctrl.Result{RequeueAfter: p.Spec.TTLAfterFinished.Duration}, nil
		}
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

func (r *ContentProjectPromotionReconciler) fail(ctx context.Context, p *uyuniv1.ContentProjectPromotion, err error) (ctrl.Result, error) {
	now := metav1.NewTime(r.Now())
	p.Status.Phase = "Failed"
	p.Status.FailureReason = err.Error()
	p.Status.CompletedAt = &now
	setReady(&p.Status.Conditions, p.Generation, metav1.ConditionFalse, "Failed", err.Error())
	if uerr := r.Status().Update(ctx, p); uerr != nil {
		return ctrl.Result{}, uerr
	}
	if p.Spec.TTLAfterFinished.Duration > 0 {
		return ctrl.Result{RequeueAfter: p.Spec.TTLAfterFinished.Duration}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ContentProjectPromotionReconciler) pending(ctx context.Context, p *uyuniv1.ContentProjectPromotion, reason, msg string, after time.Duration) (ctrl.Result, error) {
	p.Status.Phase = "Pending"
	setReady(&p.Status.Conditions, p.Generation, metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, p); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func isTerminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}

// isTransientCLMConflict recognizes Uyuni's "already in progress" response
// to a build/promote request submitted while another CLM operation on the
// same project is still finishing server-side. Deliberately narrow (exact
// substring match on the known message) rather than treating every
// PromoteProject error as retryable - a real error (bad project/env,
// permissions, etc.) should still fail immediately, not retry forever.
func isTransientCLMConflict(err error) bool {
	return strings.Contains(err.Error(), "already in progress")
}

func (r *ContentProjectPromotionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ContentProjectPromotion{}).
		Complete(r)
}
