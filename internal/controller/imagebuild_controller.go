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

	// Wait for profile to be realized.
	if profile.Status.UyuniID == 0 {
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

	// AnnBuildNow triggers a re-schedule by clearing the current actionID.
	if ib.Annotations[uyuniv1.AnnBuildNow] == "true" {
		ib.Status.ActionID = 0
		ib.Status.BuildStatus = ""
		ib.Status.ImageID = 0
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
		actionID, err := uc.ScheduleImageBuild(ctx, profile.Spec.Label, version, buildHostID)
		_ = earliest // ScheduleImageBuild doesn't take earliest; stored for reference only
		if err != nil {
			return r.fail(ctx, &ib, "ScheduleFailed", fmt.Errorf("scheduling image build: %w", err))
		}
		ib.Status.ActionID = actionID
		ib.Status.Version = version
		ib.Status.BuildStatus = "Scheduled"
		ib.Status.ObservedGeneration = ib.Generation
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Building", "Build scheduled")
		if err := r.Status().Update(ctx, &ib); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Poll existing action.
	requeue, pollErr := r.pollAction(ctx, uc, &ib, profile.Spec.Label)
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
		if err := uc.CancelAction(ctx, ib.Status.ActionID); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
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

func (r *ImageBuildReconciler) pollAction(ctx context.Context, uc uyuni.API, ib *uyuniv1.ImageBuild, profileLabel string) (time.Duration, error) {
	action, err := uc.GetActionDetails(ctx, ib.Status.ActionID)
	if err != nil {
		return 0, err
	}
	switch action.Status {
	case "Completed":
		ib.Status.BuildStatus = "Succeeded"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionTrue, "Succeeded", "")
		// Try to find the resulting image ID.
		imgs, listErr := uc.ListImagesForProfile(ctx, profileLabel)
		if listErr == nil {
			for _, img := range imgs {
				if img.Version == ib.Status.Version {
					ib.Status.ImageID = img.ID
					break
				}
			}
		}
		return 10 * time.Minute, nil
	case "Failed":
		ib.Status.BuildStatus = "Failed"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Failed", action.Name)
		return 10 * time.Minute, nil
	default:
		ib.Status.BuildStatus = "Running"
		setReady(&ib.Status.Conditions, ib.Generation, metav1.ConditionFalse, "Building", "Build is running")
		return 30 * time.Second, nil
	}
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
