package controller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type ImageProfileReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imageprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imageprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imageprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ImageProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ip uyuniv1.ImageProfile
	if err := r.Get(ctx, req.NamespacedName, &ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ip.Spec.OrganizationRef), ip.Namespace)
	if err != nil {
		return r.fail(ctx, &ip, "OrganizationError", err)
	}

	if !ip.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &ip)
	}
	if ensureFinalizer(&ip, ipFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ip)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &ip, orgRef(ip.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve ImageStore label.
	storeLabel, waitReason, err := r.resolveStoreLabel(ctx, &ip)
	if err != nil {
		return r.fail(ctx, &ip, "ResolveStoreFailed", err)
	}
	if waitReason != "" {
		setReady(&ip.Status.Conditions, ip.Generation, metav1.ConditionFalse, "WaitingForStore", waitReason)
		_ = r.Status().Update(ctx, &ip)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve activation key (optional).
	activationKey := ""
	if ip.Spec.ActivationKeyRef != nil {
		activationKey, err = r.resolveActivationKey(ctx, &ip)
		if err != nil {
			return r.fail(ctx, &ip, "ResolveActivationKeyFailed", err)
		}
	}

	// Build the source URL, optionally injecting Basic Auth credentials.
	sourceURL, err := r.buildAuthenticatedURL(ctx, &ip)
	if err != nil {
		return r.fail(ctx, &ip, "BuildURLFailed", err)
	}

	// Ensure the Uyuni image profile exists and is up-to-date.
	current, err := uc.GetImageProfile(ctx, ip.Spec.Label)
	if uyuni.IsNotFound(err) {
		if createErr := uc.CreateImageProfile(ctx, uyuni.ImageProfileDetails{
			Label:         ip.Spec.Label,
			Type:          ip.Spec.Type,
			StoreLabel:    storeLabel,
			ActivationKey: activationKey,
			SourcePath:    sourceURL,
		}, ip.Spec.CustomInfo); createErr != nil {
			return r.fail(ctx, &ip, "CreateFailed", createErr)
		}
		current, err = uc.GetImageProfile(ctx, ip.Spec.Label)
		if err != nil {
			return r.fail(ctx, &ip, "GetAfterCreate", err)
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	if profileNeedsUpdate(current, storeLabel, activationKey, sourceURL) {
		updatePayload := map[string]any{
			"storeLabel":    storeLabel,
			"activationKey": activationKey,
			"path":          sourceURL,
		}
		if updateErr := uc.UpdateImageProfile(ctx, ip.Spec.Label, updatePayload); updateErr != nil {
			return r.fail(ctx, &ip, "UpdateFailed", updateErr)
		}
	}

	ip.Status.UyuniID = current.ID

	// Handle build triggers.
	requeue, buildErr := r.handleBuildTriggers(ctx, uc, &ip)
	if buildErr != nil {
		return r.fail(ctx, &ip, "BuildTriggerFailed", buildErr)
	}

	// Poll in-progress build.
	if ip.Status.LastBuild != nil &&
		(ip.Status.LastBuild.Status == "Queued" || ip.Status.LastBuild.Status == "Running") {
		pollErr := r.pollBuild(ctx, uc, &ip)
		if pollErr != nil {
			// Non-fatal: log but continue so we don't lose status updates.
			ctrl.LoggerFrom(ctx).Error(pollErr, "polling build status")
		}
		requeue = 30 * time.Second
	}

	ip.Status.ObservedGeneration = ip.Generation
	setReady(&ip.Status.Conditions, ip.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &ip); err != nil {
		return ctrl.Result{}, err
	}

	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *ImageProfileReconciler) handleDeletion(ctx context.Context, uc uyuni.API, ip *uyuniv1.ImageProfile) (ctrl.Result, error) {
	if !containsFinalizer(ip, ipFinalizer) {
		return ctrl.Result{}, nil
	}
	// Delete by label, tolerating NotFound. image.profile.getDetails returns no
	// numeric id, so status.UyuniID may be 0 — don't gate deletion on it, or we
	// would orphan the Uyuni profile.
	if err := uc.DeleteImageProfile(ctx, ip.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(ip, ipFinalizer)
	return ctrl.Result{}, r.Update(ctx, ip)
}

// defaultOSImageStoreLabel is Uyuni's built-in OS image store. It is hidden
// from the image store list, but kiwi (OS image) profiles must reference it by
// this well-known label. Used when a kiwi ImageProfile sets no explicit storeRef.
const defaultOSImageStoreLabel = "SUSE Manager OS Image Store"

func (r *ImageProfileReconciler) resolveStoreLabel(ctx context.Context, ip *uyuniv1.ImageProfile) (label, wait string, err error) {
	// storeRef is optional for kiwi: with no reference, use Uyuni's built-in OS
	// image store, which must be passed by its well-known label.
	if ip.Spec.StoreRef == nil {
		return defaultOSImageStoreLabel, "", nil
	}
	var store uyuniv1.ImageStore
	if err := r.Get(ctx, types.NamespacedName{Namespace: ip.Namespace, Name: ip.Spec.StoreRef.Name}, &store); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", fmt.Sprintf("ImageStore %q not found", ip.Spec.StoreRef.Name), nil
		}
		return "", "", err
	}
	if store.Status.UyuniID == 0 {
		return "", fmt.Sprintf("ImageStore %q not yet realized in Uyuni", ip.Spec.StoreRef.Name), nil
	}
	return store.Spec.Label, "", nil
}

func (r *ImageProfileReconciler) resolveActivationKey(ctx context.Context, ip *uyuniv1.ImageProfile) (string, error) {
	var ak uyuniv1.ActivationKey
	if err := r.Get(ctx, types.NamespacedName{Namespace: ip.Namespace, Name: ip.Spec.ActivationKeyRef.Name}, &ak); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", nil // advisory: activation key not found yet, use empty
		}
		return "", err
	}
	return ak.Status.UyuniKey, nil
}

// buildAuthenticatedURL constructs the source URL from spec.url or spec.git,
// then optionally injects Basic Auth credentials from spec.auth.
func (r *ImageProfileReconciler) buildAuthenticatedURL(ctx context.Context, ip *uyuniv1.ImageProfile) (string, error) {
	raw := buildSourceURL(&ip.Spec)
	if ip.Spec.Auth == nil {
		return raw, nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ip.Namespace, Name: ip.Spec.Auth.Name}, &secret); err != nil {
		return "", fmt.Errorf("reading auth secret %q: %w", ip.Spec.Auth.Name, err)
	}
	usernameKey := ip.Spec.Auth.UsernameKey
	if usernameKey == "" {
		usernameKey = "username"
	}
	passwordKey := ip.Spec.Auth.PasswordKey
	if passwordKey == "" {
		passwordKey = "password"
	}
	username := string(secret.Data[usernameKey])
	password := string(secret.Data[passwordKey])
	return injectBasicAuth(raw, username, password)
}

func (r *ImageProfileReconciler) handleBuildTriggers(ctx context.Context, uc uyuni.API, ip *uyuniv1.ImageProfile) (time.Duration, error) {
	annBuildNow := ip.Annotations[uyuniv1.AnnBuildNow] == "true"
	onChange := ip.Spec.BuildPolicy == "onChange" &&
		(ip.Status.LastBuild == nil || ip.Status.ObservedGeneration < ip.Generation)

	if !annBuildNow && !onChange {
		return 0, nil
	}

	// Resolve build host.
	if ip.Spec.BuildHostRef == nil {
		if annBuildNow {
			return 0, fmt.Errorf("spec.buildHostRef is required to trigger a build")
		}
		return 0, nil // onChange without buildHost: skip silently
	}
	buildHostID, err := r.resolveBuildHostID(ctx, ip)
	if err != nil {
		return 0, err
	}
	if buildHostID == 0 {
		return 30 * time.Second, nil // host not yet registered
	}

	version := ip.Annotations[uyuniv1.AnnBuildVersion]
	if version == "" {
		version = time.Now().UTC().Format("20060102-1504")
	}

	trigger := "onChange"
	if annBuildNow {
		trigger = "annotation"
	}

	actionID, err := uc.ScheduleImageBuild(ctx, ip.Spec.Label, version, buildHostID)
	if err != nil {
		return 0, fmt.Errorf("scheduling image build: %w", err)
	}

	now := metav1.Now()
	ip.Status.LastBuild = &uyuniv1.ImageBuildRecord{
		BuildID:   actionID,
		Version:   version,
		Status:    "Queued",
		StartedAt: &now,
		Trigger:   trigger,
	}

	// Strip AnnBuildNow.
	if annBuildNow {
		patch := client.MergeFrom(ip.DeepCopy())
		delete(ip.Annotations, uyuniv1.AnnBuildNow)
		if err := r.Patch(ctx, ip, patch); err != nil {
			return 0, fmt.Errorf("stripping build-now annotation: %w", err)
		}
	}
	return 30 * time.Second, nil
}

func (r *ImageProfileReconciler) resolveBuildHostID(ctx context.Context, ip *uyuniv1.ImageProfile) (int, error) {
	var sys uyuniv1.System
	if err := r.Get(ctx, types.NamespacedName{Namespace: ip.Namespace, Name: ip.Spec.BuildHostRef.Name}, &sys); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return 0, nil
		}
		return 0, err
	}
	return sys.Status.UyuniServerID, nil
}

func (r *ImageProfileReconciler) pollBuild(ctx context.Context, uc uyuni.API, ip *uyuniv1.ImageProfile) error {
	if ip.Status.LastBuild == nil || ip.Status.LastBuild.BuildID == 0 {
		return nil
	}
	action, err := uc.GetActionDetails(ctx, ip.Status.LastBuild.BuildID)
	if err != nil {
		return err
	}
	switch action.Status {
	case "Completed":
		ip.Status.LastBuild.Status = "Succeeded"
		now := metav1.Now()
		ip.Status.LastBuild.CompletedAt = &now
		// Try to find the image ID in Uyuni.
		imgs, listErr := uc.ListImagesForProfile(ctx, ip.Spec.Label)
		if listErr == nil {
			for _, img := range imgs {
				if img.Version == ip.Status.LastBuild.Version {
					ip.Status.LastBuild.BuildID = img.ID
					break
				}
			}
		}
	case "Failed":
		ip.Status.LastBuild.Status = "Failed"
		now := metav1.Now()
		ip.Status.LastBuild.CompletedAt = &now
		ip.Status.LastBuild.FailureReason = action.Name
	default:
		ip.Status.LastBuild.Status = "Running"
	}
	return nil
}

func (r *ImageProfileReconciler) fail(ctx context.Context, ip *uyuniv1.ImageProfile, reason string, err error) (ctrl.Result, error) {
	setReady(&ip.Status.Conditions, ip.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ip)
	return ctrl.Result{}, err
}

func (r *ImageProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ImageProfile{}).
		Watches(&uyuniv1.ImageStore{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForStore)).
		Watches(&uyuniv1.ActivationKey{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForActivationKey)).
		Complete(r)
}

func (r *ImageProfileReconciler) profilesForStore(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ImageProfileList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ip := range list.Items {
		if ip.Spec.StoreRef != nil && ip.Spec.StoreRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ip.Namespace, Name: ip.Name},
			})
		}
	}
	return out
}

func (r *ImageProfileReconciler) profilesForActivationKey(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.ImageProfileList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ip := range list.Items {
		if ip.Spec.ActivationKeyRef != nil && ip.Spec.ActivationKeyRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ip.Namespace, Name: ip.Name},
			})
		}
	}
	return out
}

// buildSourceURL reconstructs the final source URL from spec.url or spec.git.
// Auth credentials are NOT injected here — call injectBasicAuth separately.
func buildSourceURL(spec *uyuniv1.ImageProfileSpec) string {
	if spec.Git != nil {
		g := spec.Git
		if g.Reference == "" && g.Path == "" {
			return g.Repository
		}
		u := g.Repository + "#" + g.Reference
		if g.Path != "" {
			u += ":" + g.Path
		}
		return u
	}
	return spec.URL
}

// injectBasicAuth injects username:password into the URL's authority component.
func injectBasicAuth(rawURL, username, password string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid source URL %q: %w", rawURL, err)
	}
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}

func profileNeedsUpdate(current *uyuni.ImageProfileDetails, storeLabel, activationKey, sourcePath string) bool {
	return current.StoreLabel != storeLabel ||
		current.ActivationKey != activationKey ||
		current.SourcePath != sourcePath
}
