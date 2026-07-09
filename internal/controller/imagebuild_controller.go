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

type ImageBuildReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagebuilds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagebuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imageprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch

func (r *ImageBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ib uyuniv1.ImageBuild
	if err := r.Get(ctx, req.NamespacedName, &ib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve organization via ImageProfile.
	var profile uyuniv1.ImageProfile
	if err := r.Get(ctx, types.NamespacedName{Namespace: ib.Namespace, Name: ib.Spec.ProfileRef.Name}, &profile); err != nil {
		if client.IgnoreNotFound(err) == nil {
			setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "WaitingForProfile",
				fmt.Sprintf("ImageProfile %q not found", ib.Spec.ProfileRef.Name))
			_ = r.Status().Update(ctx, &ib)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(profile.Spec.OrganizationRef), ib.Namespace)
	if err != nil {
		return r.fail(ctx, &ib, "OrganizationError", err)
	}

	if !ib.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &ib)
	}
	if ensureFinalizer(&ib, ibFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ib)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &ib, orgRef(profile.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Wait for the profile to be realized in Uyuni. Image profiles have no
	// numeric id in the Uyuni API (image.profile.getDetails returns none), so
	// status.UyuniID is always 0 — readiness is tracked via the profile's Ready
	// condition, which turns True once the operator has ensured the Uyuni profile
	// exists.
	if c := findCondition(profile.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionTrue {
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "WaitingForProfile",
			fmt.Sprintf("ImageProfile %q not yet realized in Uyuni", ib.Spec.ProfileRef.Name))
		_ = r.Status().Update(ctx, &ib)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve build host (spec overrides profile default).
	buildHostRef := ib.Spec.BuildHostRef
	if buildHostRef == nil {
		buildHostRef = profile.Spec.BuildHostRef
	}
	if buildHostRef == nil {
		return r.fail(ctx, &ib, "NoBuildHost", fmt.Errorf("spec.buildHostRef not set on ImageBuild or ImageProfile"))
	}
	buildHostID, err := r.resolveBuildHostID(ctx, ib.Namespace, buildHostRef.Name)
	if err != nil {
		return r.fail(ctx, &ib, "ResolveBuildHostFailed", err)
	}
	if buildHostID == 0 {
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "WaitingForBuildHost",
			fmt.Sprintf("System %q not yet registered", buildHostRef.Name))
		_ = r.Status().Update(ctx, &ib)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// AnnBuildNow triggers a re-schedule by clearing the current actionID and
	// the prior build's recorded result (so the timeout clock restarts too).
	if ib.Annotations[uyuniv1.AnnBuildNow] == "true" {
		ib.Status.ActionID = 0
		ib.Status.BuildStatus = ""
		ib.Status.ImageID = 0
		ib.Status.BaselineImageID = 0
		ib.Status.Name = ""
		ib.Status.Revision = 0
		ib.Status.Checksum = ""
		ib.Status.ImageURL = ""
		ib.Status.Files = nil
		ib.Status.StartedAt = nil
		patch := client.MergeFrom(ib.DeepCopy())
		delete(ib.Annotations, uyuniv1.AnnBuildNow)
		if err := r.Patch(ctx, &ib, patch); err != nil {
			return r.fail(ctx, &ib, "StripAnnotationFailed", err)
		}
	}

	// Schedule build if not yet scheduled.
	if ib.Status.ActionID == 0 {
		version := ib.Spec.Version
		if version == "" {
			version = time.Now().UTC().Format("20060102-1504")
		}
		earliest := time.Now()
		if ib.Spec.Earliest != nil {
			earliest = ib.Spec.Earliest.Time
		}
		// Snapshot the highest existing image id so we can later identify the
		// image this build creates (the first one above the baseline).
		ib.Status.BaselineImageID = maxImageID(ctx, uc)
		actionID, err := uc.ScheduleImageBuild(ctx, profile.Spec.Label, version, buildHostID, earliest)
		if err != nil {
			return r.fail(ctx, &ib, "ScheduleFailed", fmt.Errorf("scheduling image build: %w", err))
		}
		now := metav1.Now()
		ib.Status.ActionID = actionID
		ib.Status.Version = version
		ib.Status.StartedAt = &now
		ib.Status.BuildStatus = "Scheduled"
		ib.Status.ObservedGeneration = ib.Generation
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Building", "Build scheduled")
		if err := r.Status().Update(ctx, &ib); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Poll existing build.
	requeue, pollErr := r.pollAction(ctx, uc, &ib)
	if pollErr != nil {
		return r.fail(ctx, &ib, "PollFailed", pollErr)
	}

	ib.Status.ObservedGeneration = ib.Generation
	if err := r.Status().Update(ctx, &ib); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *ImageBuildReconciler) handleDeletion(ctx context.Context, uc uyuni.API, ib *uyuniv1.ImageBuild) (ctrl.Result, error) {
	if !containsFinalizer(ib, ibFinalizer) {
		return ctrl.Result{}, nil
	}
	if ib.Status.ActionID != 0 && ib.Status.BuildStatus == "Running" {
		// Best-effort cancel via the schedule namespace: the build user may lack
		// the role to cancel actions (403). Don't block CR cleanup on it — the
		// build finishing on its own is harmless.
		if err := uc.CancelAction(ctx, ib.Status.ActionID); err != nil && !uyuni.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Error(err, "cancelling image build action (continuing with deletion)")
		}
	}
	removeFinalizer(ib, ibFinalizer)
	return ctrl.Result{}, r.Update(ctx, ib)
}

func (r *ImageBuildReconciler) resolveBuildHostID(ctx context.Context, namespace, name string) (int, error) {
	var sys uyuniv1.System
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sys); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return 0, nil
		}
		return 0, err
	}
	return sys.Status.UyuniServerID, nil
}

// defaultBuildTimeoutMinutes is the fallback build timeout when spec.timeoutMinutes
// is unset. See pollAction for why a timeout backstop is kept.
const defaultBuildTimeoutMinutes = 120

// pollAction reports build progress from the build *action* (schedule namespace,
// GET). This is the authoritative signal: it reports Completed/Failed even for a
// build that fails before an image record is ever created (e.g. the build host
// minion is down), which is invisible to the image API. image.listImages surfaces
// only successfully-built images and its version is the kiwi version (not the tag
// we pass), so it can't be used to detect status or match a build.
//
// For a completed build we enrich status with the built image's name, version
// (from kiwi), revision, and uploaded files, found via the image id captured just
// above the baseline recorded at schedule time.
func (r *ImageBuildReconciler) pollAction(ctx context.Context, uc uyuni.API, ib *uyuniv1.ImageBuild) (time.Duration, error) {
	action, err := uc.GetActionDetails(ctx, ib.Status.ActionID)
	if err != nil {
		return 0, err
	}

	// Best-effort: locate this build's image record (the first image created
	// above the schedule-time baseline) so we can report its details. A build
	// that fails early never creates one, so ImageID stays 0 and we rely solely
	// on the action status.
	if ib.Status.ImageID == 0 {
		if imgs, listErr := uc.ListImages(ctx); listErr == nil {
			best := 0
			for _, img := range imgs {
				if img.ID > ib.Status.BaselineImageID && (best == 0 || img.ID < best) {
					best = img.ID
				}
			}
			ib.Status.ImageID = best
		}
	}

	switch action.Status {
	case "Completed":
		ib.Status.BuildStatus = "Succeeded"
		r.recordImageDetails(ctx, uc, ib)
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionTrue, "Succeeded", "")
		return 10 * time.Minute, nil
	case "Failed":
		ib.Status.BuildStatus = "Failed"
		msg := action.Name
		if msg == "" {
			msg = "image build failed"
		}
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Failed", msg)
		return 10 * time.Minute, nil
	}

	// Non-terminal. Timeout backstop for a build the action never reports as
	// finished (e.g. stuck "picked up"). Falls back to the CR creation time for
	// builds scheduled before startedAt was recorded.
	timeout := time.Duration(defaultBuildTimeoutMinutes) * time.Minute
	if ib.Spec.TimeoutMinutes > 0 {
		timeout = time.Duration(ib.Spec.TimeoutMinutes) * time.Minute
	}
	start := ib.CreationTimestamp.Time
	if ib.Status.StartedAt != nil {
		start = ib.Status.StartedAt.Time
	}
	if !start.IsZero() && time.Since(start) > timeout {
		ib.Status.BuildStatus = "Failed"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "BuildTimeout",
			fmt.Sprintf("build action (id %d) did not finish within %s — check the build host", ib.Status.ActionID, timeout))
		return 10 * time.Minute, nil
	}

	if ib.Status.ImageID == 0 {
		ib.Status.BuildStatus = "Scheduled"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Building", "Waiting for build to start")
	} else {
		ib.Status.BuildStatus = "Running"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Building", "Build is running")
	}
	return 30 * time.Second, nil
}

// recordImageDetails fills in the built image's name, version, revision, checksum
// and uploaded files from image.getDetails. Best-effort: a build whose image
// record we could not identify (ImageID == 0) still reports Succeeded.
func (r *ImageBuildReconciler) recordImageDetails(ctx context.Context, uc uyuni.API, ib *uyuniv1.ImageBuild) {
	if ib.Status.ImageID == 0 {
		return
	}
	d, err := uc.GetImageDetails(ctx, ib.Status.ImageID)
	if err != nil || d == nil {
		return
	}
	ib.Status.Name = d.Name
	if d.Version != "" {
		ib.Status.Version = d.Version
	}
	ib.Status.Revision = d.Revision
	ib.Status.Checksum = d.Checksum
	ib.Status.ImageURL = ""
	files := make([]uyuniv1.ImageFile, 0, len(d.Files))
	for _, f := range d.Files {
		files = append(files, uyuniv1.ImageFile{Name: f.Name, Type: f.Type, URL: f.URL})
		if f.Type == "image" {
			ib.Status.ImageURL = f.URL
		}
	}
	ib.Status.Files = files
}

// maxImageID returns the highest image id currently known to Uyuni, or 0.
func maxImageID(ctx context.Context, uc uyuni.API) int {
	imgs, err := uc.ListImages(ctx)
	if err != nil {
		return 0
	}
	max := 0
	for _, img := range imgs {
		if img.ID > max {
			max = img.ID
		}
	}
	return max
}

func (r *ImageBuildReconciler) fail(ctx context.Context, ib *uyuniv1.ImageBuild, reason string, err error) (ctrl.Result, error) {
	setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ib)
	return ctrl.Result{}, err
}

func (r *ImageBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ImageBuild{}).
		Watches(&uyuniv1.ImageProfile{},
			handler.EnqueueRequestsFromMapFunc(r.buildsForProfile)).
		Complete(r)
}

func (r *ImageBuildReconciler) buildsForProfile(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ImageBuildList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ib := range list.Items {
		if ib.Spec.ProfileRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ib.Namespace, Name: ib.Name},
			})
		}
	}
	return out
}
