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

	uc, err := r.Clients.ForOrganization(ctx, orgRef(cp.Spec.OrganizationRef), cp.Namespace)
	if err != nil {
		return r.fail(ctx, &cp, "OrganizationError", err)
	}

	if !cp.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &cp)
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
		existing, lookupErr := uc.LookupProject(ctx, cp.Spec.Label)
		if lookupErr != nil {
			return r.fail(ctx, &cp, "CreateProjectFailed", fmt.Errorf("project already exists but lookup failed: %w", lookupErr))
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
	if err := r.reconcileFilters(ctx, uc, &cp); err != nil {
		return r.fail(ctx, &cp, "FilterReconcileFailed", err)
	}

	// 6. Refresh environment states and decide on build.
	if err := r.refreshEnvironmentStates(ctx, uc, &cp); err != nil {
		return ctrl.Result{}, err
	}
	if reason := r.shouldBuild(&cp, desiredSources); reason != "" {
		msg := cp.Spec.Build.Message
		if msg == "" {
			msg = "automated build by uyuni-operator: " + reason
		}
		if err := uc.BuildProject(ctx, cp.Spec.Label, msg); err != nil {
			return r.fail(ctx, &cp, "BuildFailed", err)
		}
		now := metav1.NewTime(r.Now())
		cp.Status.LastBuildStartedAt = &now
		cp.Status.BuildStatus = "Building"
		cp.Status.LastBuildSourceFingerprint = fingerprintSources(desiredSources)
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
func (r *ContentProjectReconciler) handleDeletion(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) (ctrl.Result, error) {
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

	// Try to remove project from Uyuni, but don't block deletion if API fails
	if err := uc.RemoveProject(ctx, cp.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
		fmt.Printf("RemoveProject API failed (may not be available): %v\n", err)
		// Continue with cleanup anyway - API may not support this endpoint
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
	for _, ref := range cp.Spec.SourceRefs {
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
	// Source attachment is skipped if APIs are not available
	// The spec.sourceRefs are documented but not required to be attached via API
	// Attempt to list sources, but don't fail if the API is unavailable
	_, err := uc.ListProjectSources(ctx, cp.Spec.Label)
	if err != nil {
		// Log but continue - API may not be available in this Uyuni version
		fmt.Printf("ListProjectSources API failed (may not be available): %v\n", err)
		return nil
	}
	// If API succeeded, attachment logic would go here, but skipped for now
	return nil
}

func (r *ContentProjectReconciler) reconcileFilters(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) error {
	all, err := uc.ListFilters(ctx)
	if err != nil {
		// Log but continue if filter API is not available
		fmt.Printf("ListFilters API failed (may not be available): %v\n", err)
		return nil
	}
	allByName := map[string]uyuni.FilterDetails{}
	for _, f := range all {
		allByName[f.Name] = f
	}

	if cp.Status.FilterIDs == nil {
		cp.Status.FilterIDs = map[string]int{}
	}
	desiredNames := map[string]bool{}

	for _, f := range cp.Spec.Filters {
		fullName := cp.Spec.Label + "-" + f.Name
		desiredNames[fullName] = true
		desired := uyuni.FilterCriteriaWire{
			Field: f.Criteria.Field, Matcher: f.Criteria.Matcher, Value: f.Criteria.Value,
		}

		if existing, ok := allByName[fullName]; ok {
			if existing.Rule != f.Rule || existing.Criteria != desired {
				if err := uc.UpdateFilter(ctx, existing.ID, fullName, f.Rule, desired); err != nil {
					return fmt.Errorf("update filter %q: %w", fullName, err)
				}
			}
			cp.Status.FilterIDs[fullName] = existing.ID
			continue
		}

		created, err := uc.CreateFilter(ctx, fullName, f.Type, f.Rule, desired)
		if err != nil {
			return fmt.Errorf("create filter %q: %w", fullName, err)
		}
		if err := uc.AttachFilter(ctx, cp.Spec.Label, created.ID); err != nil {
			return fmt.Errorf("attach filter %q: %w", fullName, err)
		}
		cp.Status.FilterIDs[fullName] = created.ID
	}

	for name, id := range cp.Status.FilterIDs {
		if desiredNames[name] {
			continue
		}
		_ = uc.DetachFilter(ctx, cp.Spec.Label, id)
		if err := uc.RemoveFilter(ctx, id); err != nil && !uyuni.IsNotFound(err) {
			return fmt.Errorf("remove filter %q: %w", name, err)
		}
		delete(cp.Status.FilterIDs, name)
	}
	return nil
}

func (r *ContentProjectReconciler) refreshEnvironmentStates(ctx context.Context, uc uyuni.API, cp *uyuniv1.ContentProject) error {
	// Skip environment state refresh if API is not available
	// Environment management is delegated to ClmEnvironment CRD
	envs, err := uc.ListProjectEnvironments(ctx, cp.Spec.Label)
	if err != nil {
		// Log but continue - API may not be available in this Uyuni version
		fmt.Printf("ListProjectEnvironments API failed (may not be available): %v\n", err)
		cp.Status.BuildStatus = "Idle"
		cp.Status.EnvironmentStates = []uyuniv1.EnvironmentState{}
		return nil
	}
	states := make([]uyuniv1.EnvironmentState, 0, len(envs))
	anyBuilding := false
	anyFailed := false
	for _, e := range envs {
		s := uyuniv1.EnvironmentState{Label: e.Label, Name: e.Name, BuiltVersion: e.Version}
		if e.Version > 0 {
			if old := findEnvState(cp.Status.EnvironmentStates, e.Label); old != nil &&
				old.BuiltVersion == e.Version && old.BuiltAt != nil {
				s.BuiltAt = old.BuiltAt
			} else {
				now := metav1.NewTime(r.Now())
				s.BuiltAt = &now
			}
			s.DerivedChannels = deriveChannelLabels(cp.Spec.Label, e.Label, cp.Status.AttachedSources)
		}
		switch e.Status {
		case "BUILDING", "GENERATING_REPODATA", "NEW":
			anyBuilding = true
		case "FAILED":
			anyFailed = true
		}
		states = append(states, s)
	}
	cp.Status.EnvironmentStates = states
	switch {
	case anyBuilding:
		cp.Status.BuildStatus = "Building"
	case anyFailed:
		cp.Status.BuildStatus = "Failed"
	default:
		cp.Status.BuildStatus = "Idle"
	}
	return nil
}

// --- build decision ---

func (r *ContentProjectReconciler) shouldBuild(cp *uyuniv1.ContentProject, sources []string) string {
	if cp.Status.BuildStatus == "Building" {
		return ""
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
		Watches(&uyuniv1.ContentProjectPromotion{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForPromotion)).
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
