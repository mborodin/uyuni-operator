package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// ContentProjectReconciler manages the lifecycle of a Uyuni Content
// Management Project: sources, environments, filters, builds.
//
// Structural spec validation (env chain shape, cron syntax, etc.) lives
// in the validating webhook; this reconciler trusts that what's in etcd
// is structurally valid and focuses on convergence.
type ContentProjectReconciler struct {
	client.Client
	Clients uyuni.ClientPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojectpromotions,verbs=get;list;watch

func (r *ContentProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cp uyuniv1.ContentProject
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cp.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &cp)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(cp.Spec.OrganizationRef), cp.Namespace)
	if err != nil {
		return r.fail(ctx, &cp, "OrganizationError", err)
	}

	if ensureFinalizer(&cp, cpFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cp)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &cp, orgRef(cp.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// 1. Resolve source channel refs. Partial readiness is OK — we still
	// reconcile everything else and degrade gracefully.
	desiredSources, missing, err := r.resolveSources(ctx, &cp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2. Project create/lookup (idempotent)
	created, err := uc.CreateProject(ctx, cp.Spec.Label, cp.Spec.Name, cp.Spec.Description)
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return r.fail(ctx, &cp, "CreateProjectFailed", err)
		}
		// Project already exists — look it up and check for a duplicate CR before adopting.
		existing, lookupErr := uc.LookupProject(ctx, cp.Spec.Label)
		if lookupErr != nil {
			return r.fail(ctx, &cp, "LookupProjectFailed", lookupErr)
		}
		var list uyuniv1.ContentProjectList
		if listErr := r.List(ctx, &list, client.InNamespace(cp.Namespace)); listErr != nil {
			return ctrl.Result{}, listErr
		}
		for _, other := range list.Items {
			if other.Name != cp.Name && other.Spec.Label == cp.Spec.Label && other.Status.ObservedGeneration > 0 {
				setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse,
					"ProjectLabelConflict", fmt.Sprintf("content project %q is already managed by ContentProject CR %q; rename this project or delete the existing one first", cp.Spec.Label, other.Name))
				_ = r.Status().Update(ctx, &cp)
				return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
			}
		}
		cp.Status.UyuniID = existing.ID
	} else {
		cp.Status.UyuniID = created.ID
	}

	// 3. Environment chain. Webhook validated structure; we walk it
	// trusting the shape. ChainOrder relies on that trust.
	if err := r.reconcileEnvironments(ctx, uc, &cp); err != nil {
		return r.fail(ctx, &cp, "EnvironmentReconcileFailed", err)
	}

	// 4. Sources.
	if err := r.reconcileSources(ctx, uc, &cp, desiredSources); err != nil {
		return r.fail(ctx, &cp, "SourceReconcileFailed", err)
	}
	cp.Status.AttachedSources = append([]string(nil), desiredSources...)

	// 5. Filters.
	filtersChanged, err := r.reconcileFilters(ctx, uc, &cp)
	if err != nil {
		return r.fail(ctx, &cp, "FilterReconcileFailed", err)
	}

	// 5b. Cleanup orphaned filters (not used by any project)
	r.cleanupOrphanedFilters(ctx, uc)

	// 6. Refresh environment states and decide on build.
	if err := r.refreshEnvironmentStates(ctx, uc, &cp); err != nil {
		return ctrl.Result{}, err
	}
	if reason := r.shouldBuild(ctx, &cp, desiredSources, filtersChanged); reason != "" {
		msg := cp.Spec.Build.Message
		if msg == "" {
			msg = "automated build by uyuni-operator: " + reason
		}
		fmt.Printf("Triggering build for project %q (reason: %s)\n", cp.Spec.Label, reason)
		if err := uc.BuildProject(ctx, cp.Spec.Label, msg); err != nil {
			// If build fails due to no environments, skip and retry later (environments may be still being created)
			if strings.Contains(err.Error(), "no environments") {
				fmt.Printf("Build skipped for project %q - waiting for environments to be created: %v\n", cp.Spec.Label, err)
			} else {
				fmt.Printf("Build failed for project %q: %v\n", cp.Spec.Label, err)
				return r.fail(ctx, &cp, "BuildFailed", err)
			}
		} else {
			fmt.Printf("Build triggered successfully for project %q\n", cp.Spec.Label)
			now := metav1.NewTime(r.Now())
			cp.Status.LastBuildStartedAt = &now
			cp.Status.BuildStatus = "Building"
			cp.Status.LastBuildSourceFingerprint = fingerprintSources(desiredSources)
		}
	}

	// 7. Status & requeue.
	cp.Status.ObservedGeneration = cp.Generation
	if len(missing) > 0 {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse,
			"PartialSources",
			fmt.Sprintf("%d source(s) not ready: %v", len(missing), missing))
	} else {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionTrue, "Reconciled", "")
	}
	if err := r.Status().Update(ctx, &cp); err != nil {
		// Handle conflict (object modified by another reconcile) gracefully
		if strings.Contains(err.Error(), "has been modified") {
			fmt.Printf("Status update conflict for project %q, requeuing: %v\n", cp.Spec.Label, err)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if cp.Status.BuildStatus == "Building" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if next := r.nextCronDeadline(&cp); !next.IsZero() {
		return ctrl.Result{RequeueAfter: time.Until(next).Round(time.Second)}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// --- deletion ---

// handleDeletion uses ownerReferences-based GC. The actual cascade of
// ActivationKeys/Systems is Kubernetes' job; we wait for owned dependents
// to finalize, then clean up Uyuni-side state. Active promotions block
// unconditionally because cancelling them mid-flight is dangerous.
func (r *ContentProjectReconciler) handleDeletion(ctx context.Context, cp *uyuniv1.ContentProject) (ctrl.Result, error) {
	if !containsFinalizer(cp, cpFinalizer) {
		return ctrl.Result{}, nil
	}

	// Escape hatch: skip Uyuni cleanup, drop finalizer. Owned dependents
	// will still be reclaimed by k8s GC; this only short-circuits OUR cleanup.
	if cp.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(cp, cpFinalizer)
		return ctrl.Result{}, r.Update(ctx, cp)
	}

	if active, err := r.activePromotions(ctx, cp); err != nil {
		return ctrl.Result{}, err
	} else if active > 0 {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse,
			"PromotionInFlight",
			fmt.Sprintf("waiting for %d active promotion(s)", active))
		_ = r.Status().Update(ctx, cp)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if pending, err := r.pendingOwnedDependents(ctx, cp); err != nil {
		return ctrl.Result{}, err
	} else if pending > 0 {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse,
			"WaitingForDependents",
			fmt.Sprintf("Kubernetes garbage collector is reclaiming %d owned resource(s)", pending))
		_ = r.Status().Update(ctx, cp)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(cp.Spec.OrganizationRef), cp.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := uc.RemoveProject(ctx, cp.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Our owned filters are project-scoped by naming convention but live in
	// the org's filter namespace. Clean up explicitly to avoid orphans.
	for _, id := range cp.Status.FilterIDs {
		_ = uc.RemoveFilter(ctx, id)
	}

	removeFinalizer(cp, cpFinalizer)
	return ctrl.Result{}, r.Update(ctx, cp)
}

func (r *ContentProjectReconciler) activePromotions(ctx context.Context, cp *uyuniv1.ContentProject) (int, error) {
	var list uyuniv1.ContentProjectPromotionList
	if err := r.List(ctx, &list, client.InNamespace(cp.Namespace)); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list.Items {
		if p.Spec.ProjectRef.Name != cp.Name {
			continue
		}
		if p.Status.Phase == "" || p.Status.Phase == "Pending" || p.Status.Phase == "Running" {
			n++
		}
	}
	return n, nil
}

func (r *ContentProjectReconciler) pendingOwnedDependents(ctx context.Context, cp *uyuniv1.ContentProject) (int, error) {
	var pending int
	var aks uyuniv1.ActivationKeyList
	if err := r.List(ctx, &aks, client.InNamespace(cp.Namespace)); err != nil {
		return 0, err
	}
	for i := range aks.Items {
		if isOwnedBy(&aks.Items[i], cp) {
			pending++
		}
	}
	var systems uyuniv1.SystemList
	if err := r.List(ctx, &systems, client.InNamespace(cp.Namespace)); err != nil {
		return 0, err
	}
	for i := range systems.Items {
		if isOwnedBy(&systems.Items[i], cp) {
			pending++
		}
	}
	return pending, nil
}

// --- resolve / reconcile helpers ---

func (r *ContentProjectReconciler) resolveSources(ctx context.Context, cp *uyuniv1.ContentProject) (labels, missing []string, err error) {
	// Collect channel refs from either channels (base+child) or sourceRefs
	var refs []uyuniv1.LocalObjectRef

	// Prefer channels configuration (base + child combined)
	if cp.Spec.Channels != nil {
		refs = append(refs, cp.Spec.Channels.BaseChannelRefs...)
		refs = append(refs, cp.Spec.Channels.ChildChannelRefs...)
	} else if len(cp.Spec.SourceRefs) > 0 {
		// Fall back to sourceRefs if channels not configured
		refs = cp.Spec.SourceRefs
	}

	for _, ref := range refs {
		var sc uyuniv1.SoftwareChannel
		if err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: ref.Name}, &sc); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, nil, err
			}
			missing = append(missing, ref.Name+" (not found)")
			continue
		}
		if sc.Status.Label == "" {
			missing = append(missing, ref.Name+" (not realized)")
			continue
		}
		labels = append(labels, sc.Status.Label)
	}
	return labels, missing, nil
}

func (r *ContentProjectReconciler) reconcileEnvironments(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) error {
	// Environment management is now delegated to ClmEnvironment CRD
	// The webhook already validates environment chain structure
	// We skip API calls here as the endpoints may not be available in all Uyuni versions
	// ClmEnvironment resources are responsible for creating/managing environments
	return nil
}

// chainOrderFromUyuni walks the chain by Uyuni's predecessor links.
// Returns the chain order it finds, even on malformed (orphaned) input —
// callers should not error here, just operate on what's there.
func chainOrderFromUyuni(envs []uyuni.ProjectEnvironmentInfo) []uyuni.ProjectEnvironmentInfo {
	byPrev := map[string]uyuni.ProjectEnvironmentInfo{}
	for _, e := range envs {
		byPrev[e.PreviousEnvironmentLabel] = e
	}
	out := make([]uyuni.ProjectEnvironmentInfo, 0, len(envs))
	cursor := ""
	visited := map[string]bool{}
	for {
		next, ok := byPrev[cursor]
		if !ok || visited[next.Label] {
			break
		}
		visited[next.Label] = true
		out = append(out, next)
		cursor = next.Label
	}
	return out
}

func (r *ContentProjectReconciler) reconcileSources(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject, desired []string) error {
	log := ctrl.LoggerFrom(ctx)
	log.Info("reconciling sources", "projectLabel", cp.Spec.Label, "desiredSourceCount", len(desired), "desiredSources", desired)

	// Get current sources from Uyuni
	current, err := uc.ListProjectSources(ctx, cp.Spec.Label)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	log.Info("fetched current sources from Uyuni", "projectLabel", cp.Spec.Label, "currentSourceCount", len(current))

	// Build sets for comparison
	desiredSet := make(map[string]bool)
	for _, label := range desired {
		desiredSet[label] = true
	}

	currentSet := make(map[string]bool)
	var currentLabels []string
	for _, source := range current {
		currentSet[source.Channel.Label] = true
		currentLabels = append(currentLabels, source.Channel.Label)
	}
	log.Info("source sets built", "projectLabel", cp.Spec.Label, "currentLabels", currentLabels)

	// Attach missing sources (with position based on desired order)
	for position, label := range desired {
		if !currentSet[label] {
			log.Info("attaching source to project", "source", label, "projectLabel", cp.Spec.Label, "position", position)
			if err := uc.AttachSourceWithPosition(ctx, cp.Spec.Label, label, position); err != nil {
				log.Error(err, "failed to attach source", "source", label, "projectLabel", cp.Spec.Label, "position", position)
				return fmt.Errorf("attach source %q: %w", label, err)
			}
			log.Info("successfully attached source", "source", label, "projectLabel", cp.Spec.Label, "position", position)
		}
	}

	// Detach removed sources
	for source := range currentSet {
		if source == "" {
			continue
		}
		if !desiredSet[source] {
			log.Info("detaching source from project", "source", source, "projectLabel", cp.Spec.Label)
			if err := uc.DetachSource(ctx, cp.Spec.Label, source); err != nil {
				log.Error(err, "failed to detach source", "source", source, "projectLabel", cp.Spec.Label)
				return fmt.Errorf("detach source %q: %w", source, err)
			}
			log.Info("successfully detached source", "source", source, "projectLabel", cp.Spec.Label)
		}
	}

	return nil
}

func (r *ContentProjectReconciler) reconcileFilters(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) (bool, error) {
	all, err := uc.ListFilters(ctx)
	if err != nil {
		// Log but continue if filter API is not available
		fmt.Printf("ListFilters API failed (may not be available): %v\n", err)
		return false, nil
	}
	allByName := map[string]uyuni.FilterDetails{}
	for _, f := range all {
		allByName[f.Name] = f
	}

	if cp.Status.FilterIDs == nil {
		cp.Status.FilterIDs = map[string]int{}
	}
	desiredNames := map[string]bool{}
	filtersChanged := false

	for _, f := range cp.Spec.Filters {
		fullName := f.Name
		desiredNames[fullName] = true
		desired := uyuni.FilterCriteriaWire{
			Field: f.Criteria.Field, Matcher: f.Criteria.Matcher, Value: f.Criteria.Value,
		}

		if existing, ok := allByName[fullName]; ok {
			// Compare field-by-field to avoid false positives from API response formatting
			ruleMatch := existing.Rule == f.Rule
			criteriaMatch := existing.Criteria.Field == desired.Field &&
				existing.Criteria.Matcher == desired.Matcher &&
				existing.Criteria.Value == desired.Value

			if !ruleMatch || !criteriaMatch {
				if err := uc.UpdateFilter(ctx, existing.ID, fullName, f.Rule, desired); err != nil {
					return false, fmt.Errorf("update filter %q: %w", fullName, err)
				}
				filtersChanged = true
			}
			cp.Status.FilterIDs[fullName] = existing.ID
			continue
		}

		created, err := uc.CreateFilter(ctx, fullName, f.Type, f.Rule, desired)
		if err != nil {
			return false, fmt.Errorf("create filter %q: %w", fullName, err)
		}
		if err := uc.AttachFilter(ctx, cp.Spec.Label, created.ID); err != nil {
			return false, fmt.Errorf("attach filter %q: %w", fullName, err)
		}
		cp.Status.FilterIDs[fullName] = created.ID
		filtersChanged = true
	}

	for name, id := range cp.Status.FilterIDs {
		if desiredNames[name] {
			continue
		}
		// Detach filter from this project
		if err := uc.DetachFilter(ctx, cp.Spec.Label, id); err != nil {
			fmt.Printf("Failed to detach filter %q from project %q: %v\n", name, cp.Spec.Label, err)
			return false, err
		}
		fmt.Printf("Filter %q detached from project %q\n", name, cp.Spec.Label)
		// Remove from THIS project's tracking immediately since detach succeeded
		delete(cp.Status.FilterIDs, name)
		filtersChanged = true

		// Try to remove the filter globally from Uyuni (best-effort)
		// Only succeeds if no other projects are using it
		if err := uc.RemoveFilter(ctx, id); err != nil {
			if uyuni.IsNotFound(err) {
				// Filter already removed
				fmt.Printf("Filter %q already removed from Uyuni\n", name)
			} else if strings.Contains(err.Error(), "still in use") || strings.Contains(err.Error(), "is used in") {
				// Check Kubernetes: is this filter used by ANY other ContentProject?
				isUsedByAnyProject := false
				usedByProjects := []string{}

				var projectList uyuniv1.ContentProjectList
				if listErr := r.List(ctx, &projectList, client.InNamespace(cp.Namespace)); listErr == nil {
					for _, proj := range projectList.Items {
						if proj.Name == cp.Name {
							// Skip this project (we just detached it)
							continue
						}
						// Check if any other project has this filter
						if _, hasFilter := proj.Status.FilterIDs[name]; hasFilter {
							isUsedByAnyProject = true
							usedByProjects = append(usedByProjects, proj.Name)
						}
					}
				}

				if isUsedByAnyProject {
					fmt.Printf("Filter %q still used by projects: %v - will be cleaned up when they remove it\n", name, usedByProjects)
				} else {
					// Filter is not used by ANY ContentProject in Kubernetes, but Uyuni says it's "in use"
					// This is likely an orphaned filter. Since it's not used, attempt permanent deletion.
					fmt.Printf("Filter %q not used by any ContentProject (orphaned) - forcing permanent deletion...\n", name)
					if forceErr := uc.RemoveFilter(ctx, id); forceErr != nil {
						fmt.Printf("Force delete filter %q failed: %v - may need manual removal from Uyuni UI\n", name, forceErr)
					} else {
						fmt.Printf("Filter %q permanently deleted from Uyuni ✅\n", name)
					}
				}
			} else {
				// Other errors - log but don't fail (filter is already detached from this project)
				fmt.Printf("RemoveFilter %q: %v (filter already removed from project)\n", name, err)
			}
		} else {
			// Filter successfully removed from Uyuni ✅
			fmt.Printf("Filter %q removed completely from Uyuni\n", name)
		}
	}
	return filtersChanged, nil
}

func (r *ContentProjectReconciler) refreshEnvironmentStates(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) error {
	// List ClmEnvironment CRs for this project (source of truth in Kubernetes)
	var envList uyuniv1.ClmEnvironmentList
	if err := r.List(ctx, &envList, client.InNamespace(cp.Namespace)); err != nil {
		fmt.Printf("Failed to list ClmEnvironments for project %q: %v\n", cp.Spec.Label, err)
		cp.Status.EnvironmentStates = []uyuniv1.EnvironmentState{}
		cp.Status.BuildStatus = "Idle"
		return nil
	}

	// Filter to only environments for this project
	states := make([]uyuniv1.EnvironmentState, 0)
	for _, env := range envList.Items {
		if env.Spec.ProjectRef.Name == cp.Name {
			s := uyuniv1.EnvironmentState{
				Label: env.Spec.Id,
				Name:  env.Spec.Name,
			}
			states = append(states, s)
		}
	}

	cp.Status.EnvironmentStates = states
	fmt.Printf("Project %q: found %d ClmEnvironments in Kubernetes\n", cp.Spec.Label, len(states))
	cp.Status.BuildStatus = "Idle"
	return nil
}

// --- build decision ---

func (r *ContentProjectReconciler) isProjectReady(cp *uyuniv1.ContentProject) bool {
	// Project is ready if at least one environment exists in status
	// (Environments are managed as separate ClmEnvironment CRs, not in ContentProject spec)
	if len(cp.Status.EnvironmentStates) == 0 {
		fmt.Printf("Project %q not ready: no environments created yet\n", cp.Spec.Label)
		return false
	}
	return true
}

func (r *ContentProjectReconciler) areFiltersReady(cp *uyuniv1.ContentProject) bool {
	// Filters are ready if all spec filters have been created in Uyuni (have IDs)
	if len(cp.Spec.Filters) == 0 {
		return true // No filters defined = ready
	}
	if len(cp.Status.FilterIDs) != len(cp.Spec.Filters) {
		fmt.Printf("Project %q not ready: %d/%d filters created in Uyuni\n",
			cp.Spec.Label, len(cp.Status.FilterIDs), len(cp.Spec.Filters))
		return false
	}
	return true
}

func (r *ContentProjectReconciler) areEnvironmentsReady(ctx context.Context, cp *uyuniv1.ContentProject) bool {
	// Environments are ready if all ClmEnvironment CRs for this project are Ready
	var envList uyuniv1.ClmEnvironmentList
	if err := r.List(ctx, &envList, client.InNamespace(cp.Namespace)); err != nil {
		fmt.Printf("Project %q: failed to list ClmEnvironments: %v\n", cp.Spec.Label, err)
		return false
	}

	readyCount := 0
	for _, env := range envList.Items {
		if env.Spec.ProjectRef.Name == cp.Name {
			// Check if this environment is Ready
			for _, cond := range env.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
					readyCount++
					break
				}
			}
		}
	}

	expectedCount := len(cp.Status.EnvironmentStates)
	if readyCount < expectedCount {
		fmt.Printf("Project %q: waiting for environments - %d/%d ready in Uyuni\n",
			cp.Spec.Label, readyCount, expectedCount)
		return false
	}
	return true
}

func (r *ContentProjectReconciler) cleanupOrphanedFilters(ctx context.Context, uc uyuni.API) {
	// Periodically clean up filters that aren't attached to any ContentProject
	allFilters, err := uc.ListFilters(ctx)
	if err != nil {
		fmt.Printf("Failed to list all filters for cleanup: %v\n", err)
		return
	}

	// Get all ContentProjects to check which filters are in use
	var projectList uyuniv1.ContentProjectList
	if err := r.List(ctx, &projectList); err != nil {
		fmt.Printf("Failed to list ContentProjects for filter cleanup: %v\n", err)
		return
	}

	// Build set of in-use filter names from all projects
	inUseFilters := make(map[string]bool)
	for _, proj := range projectList.Items {
		for filterName := range proj.Status.FilterIDs {
			inUseFilters[filterName] = true
		}
	}

	// Delete any filter not in use by any project
	for _, filter := range allFilters {
		if !inUseFilters[filter.Name] {
			fmt.Printf("Filter %q (ID: %d) is orphaned - attempting cleanup...\n", filter.Name, filter.ID)
			if err := uc.RemoveFilter(ctx, filter.ID); err != nil {
				fmt.Printf("Failed to remove orphaned filter %q: %v\n", filter.Name, err)
			} else {
				fmt.Printf("Orphaned filter %q removed successfully ✅\n", filter.Name)
			}
		}
	}
}

func (r *ContentProjectReconciler) shouldBuild(ctx context.Context, cp *uyuniv1.ContentProject, sources []string, filtersChanged bool) string {
	if cp.Status.BuildStatus == "Building" {
		return ""
	}
	if filtersChanged {
		// Filter just changed - trigger build immediately if ready
		if !r.isProjectReady(cp) {
			fmt.Printf("Project %q: filters changed but waiting for project to be ready (environments)\n", cp.Spec.Label)
			return ""
		}
		if !r.areFiltersReady(cp) {
			fmt.Printf("Project %q: filters changed but waiting for filters to be ready (Uyuni)\n", cp.Spec.Label)
			return ""
		}
		fmt.Printf("Project %q: filters ready, triggering build now\n", cp.Spec.Label)
		return "filters-changed"
	}

	// Check if this is the first build (filters exist but never built)
	hasFilters := len(cp.Spec.Filters) > 0
	filtersReady := r.areFiltersReady(cp)
	neverBuilt := cp.Status.LastBuildStartedAt == nil

	if hasFilters && filtersReady && neverBuilt && r.isProjectReady(cp) {
		// First build: all filters ready and never built before
		// But wait for environments to be created in Uyuni first
		if !r.areEnvironmentsReady(ctx, cp) {
			fmt.Printf("Project %q: filters ready but waiting for environments to be created in Uyuni\n", cp.Spec.Label)
			return ""
		}
		fmt.Printf("Project %q: all filters and environments ready, triggering first build now\n", cp.Spec.Label)
		return "initial-filters"
	}
	if cp.Spec.Build.AutoBuildSources {
		fp := fingerprintSources(sources)
		if fp != cp.Status.LastBuildSourceFingerprint {
			return "source-content-changed"
		}
	}
	if cp.Spec.Build.Schedule != "" {
		next := r.nextCronDeadline(cp)
		if !next.IsZero() && r.Now().After(next) {
			return "cron"
		}
	}
	return ""
}

func (r *ContentProjectReconciler) nextCronDeadline(cp *uyuniv1.ContentProject) time.Time {
	if cp.Spec.Build.Schedule == "" {
		return time.Time{}
	}
	sched, err := cron.ParseStandard(cp.Spec.Build.Schedule)
	if err != nil {
		// Webhook should have caught this; ignore quietly.
		return time.Time{}
	}
	from := cp.CreationTimestamp.Time
	if cp.Status.LastBuildStartedAt != nil {
		from = cp.Status.LastBuildStartedAt.Time
	}
	return sched.Next(from)
}

func fingerprintSources(labels []string) string {
	sort.Strings(labels)
	h := sha256.New()
	for _, l := range labels {
		h.Write([]byte(l))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func deriveChannelLabels(projectLabel, envLabel string, sourceLabels []string) []string {
	out := make([]string, 0, len(sourceLabels))
	for _, s := range sourceLabels {
		out = append(out, projectLabel+"-"+envLabel+"-"+s)
	}
	return out
}

func findEnvState(states []uyuniv1.EnvironmentState, label string) *uyuniv1.EnvironmentState {
	for i := range states {
		if states[i].Label == label {
			return &states[i]
		}
	}
	return nil
}

// --- error path & watches ---

func (r *ContentProjectReconciler) fail(ctx context.Context, cp *uyuniv1.ContentProject, reason string, err error) (ctrl.Result, error) {
	setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, cp)
	return ctrl.Result{}, err
}

func (r *ContentProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ContentProject{}).
		Watches(&uyuniv1.SoftwareChannel{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForChannel)).
		Watches(&uyuniv1.Repository{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForRepository)).
		Watches(&uyuniv1.ContentProjectPromotion{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForPromotion)).
		Watches(&uyuniv1.ClmEnvironment{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForEnvironment)).
		Complete(r)
}

func (r *ContentProjectReconciler) projectsForChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ContentProjectList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, cp := range list.Items {
		for _, ref := range cp.Spec.SourceRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *ContentProjectReconciler) projectsForPromotion(_ context.Context, obj client.Object) []reconcile.Request {
	p, ok := obj.(*uyuniv1.ContentProjectPromotion)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.ProjectRef.Name},
	}}
}

func (r *ContentProjectReconciler) projectsForRepository(ctx context.Context, obj client.Object) []reconcile.Request {
	// When a Repository changes, trigger reconciliation of all ContentProjects in the namespace
	// This ensures sourceRefs changes cascade to trigger ContentProject reconciliation
	var list uyuniv1.ContentProjectList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, cp := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name},
		})
	}
	return out
}

func (r *ContentProjectReconciler) projectsForEnvironment(_ context.Context, obj client.Object) []reconcile.Request {
	// When a ClmEnvironment changes (especially becoming Ready), trigger reconciliation
	// of its parent ContentProject so the project can check if build conditions are now met
	env, ok := obj.(*uyuniv1.ClmEnvironment)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name},
	}}
}
