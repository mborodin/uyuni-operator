package controller

import (
	"context"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type PackageCleanupPolicyReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=packagecleanuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=packagecleanuppolicies/status,verbs=get;update;patch

// idlePollInterval is how long the controller waits before checking for new
// orphaned packages once a run finds nothing left to process. Not
// spec-configurable (yet) — the per-run tunables (MaxDeletePerRun,
// BatchIntervalSeconds) are what users actually need to adjust.
const idlePollInterval = 24 * time.Hour

// Reconcile runs one batch of the orphaned-package sweep. Unlike other
// controllers, there is no per-package Kubernetes object to watch — packages
// exist only in Uyuni — so this drives entirely off Uyuni's own API
// (channel.software.listPackagesWithoutChannel for the bulk candidate list,
// then packages.listProvidingErrata / system.listSystemsWithPackage to
// re-verify each one individually right before any deletion).
func (r *PackageCleanupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pol uyuniv1.PackageCleanupPolicy
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer: this policy has no corresponding Uyuni-side object to
	// clean up on delete. Deleting the CR simply stops future sweeps.
	if !pol.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(pol.Spec.OrganizationRef), pol.Namespace)
	if err != nil {
		return r.fail(ctx, &pol, "OrganizationError", err)
	}

	candidates, err := uc.ListPackagesWithoutChannel(ctx)
	if err != nil {
		return r.fail(ctx, &pol, "ListFailed", err)
	}

	minAge := time.Duration(pol.Spec.MinOrphanAgeDays) * 24 * time.Hour
	now := time.Now()

	// Age filter is free (no extra API call — LastModified came back with
	// the bulk list). A zero LastModified means the date couldn't be
	// parsed; treat that as "too recent" rather than guess it's old enough.
	var ageEligible []uyuni.OrphanedPackage
	skippedTooRecent := 0
	for _, p := range candidates {
		if p.LastModified.IsZero() || now.Sub(p.LastModified) < minAge {
			skippedTooRecent++
			continue
		}
		ageEligible = append(ageEligible, p)
	}

	// Oldest first, so the backlog drains in a stable order across batches
	// instead of reshuffling which packages get picked each reconcile.
	sort.Slice(ageEligible, func(i, j int) bool {
		return ageEligible[i].LastModified.Before(ageEligible[j].LastModified)
	})

	batch := ageEligible
	if len(batch) > pol.Spec.MaxDeletePerRun {
		batch = batch[:pol.Spec.MaxDeletePerRun]
	}

	skippedErrata := 0
	skippedInstalled := 0
	deleted := 0
	for _, p := range batch {
		// Re-verify live, right before acting — the bulk list can be
		// minutes old by the time we get here, and a package that was
		// errata-free or uninstalled when listed may not be by now.
		errata, err := uc.ListProvidingErrata(ctx, p.ID)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "checking errata linkage, skipping package", "packageId", p.ID, "name", p.Name)
			continue
		}
		if len(errata) > 0 {
			skippedErrata++
			continue
		}
		systems, err := uc.ListSystemsWithPackage(ctx, p.ID)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "checking installed systems, skipping package", "packageId", p.ID, "name", p.Name)
			continue
		}
		if len(systems) > 0 {
			skippedInstalled++
			continue
		}

		if pol.Spec.DryRun {
			// Logged at Info so the exact candidate list is reviewable via
			// `kubectl logs` before anyone flips dryRun to false — the
			// status counts alone don't name names.
			ctrl.LoggerFrom(ctx).Info("dry-run: would delete package",
				"packageId", p.ID, "name", p.Name, "version", p.Version, "release", p.Release, "arch", p.Arch)
			continue
		}
		if err := uc.RemovePackage(ctx, p.ID); err != nil && !uyuni.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Error(err, "removing package (continuing with next)", "packageId", p.ID, "name", p.Name)
			continue
		}
		ctrl.LoggerFrom(ctx).Info("deleted package",
			"packageId", p.ID, "name", p.Name, "version", p.Version, "release", p.Release, "arch", p.Arch)
		deleted++
	}

	runTime := metav1.Now()
	pol.Status.LastRunTime = &runTime
	pol.Status.CandidatesFound = len(candidates)
	pol.Status.SkippedTooRecent = skippedTooRecent
	pol.Status.SkippedErrataLinked = skippedErrata
	pol.Status.SkippedInstalled = skippedInstalled
	pol.Status.DeletedThisRun = deleted
	pol.Status.RemainingCandidates = len(ageEligible) - len(batch)
	pol.Status.ObservedGeneration = pol.Generation
	setReady(&pol.Status.Conditions, pol.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &pol); err != nil {
		return ctrl.Result{}, err
	}

	// More eligible candidates than this batch covered: come back quickly
	// rather than waiting for the idle interval, so a large backlog drains
	// over consecutive batches in one sitting instead of one per day.
	if pol.Status.RemainingCandidates > 0 {
		return ctrl.Result{RequeueAfter: time.Duration(pol.Spec.BatchIntervalSeconds) * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: idlePollInterval}, nil
}

func (r *PackageCleanupPolicyReconciler) fail(ctx context.Context, pol *uyuniv1.PackageCleanupPolicy, reason string, err error) (ctrl.Result, error) {
	setReady(&pol.Status.Conditions, pol.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, pol)
	return ctrl.Result{}, err
}

func (r *PackageCleanupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.PackageCleanupPolicy{}).
		Complete(r)
}
