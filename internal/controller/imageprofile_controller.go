package controller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagebuilds,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ImageProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ip uyuniv1.ImageProfile
	if err := r.Get(ctx, req.NamespacedName, &ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ip.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ip)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ip.Spec.OrganizationRef), ip.Namespace)
	if err != nil {
		return r.fail(ctx, &ip, "OrganizationError", err)
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
			KiwiOptions:   ip.Spec.KiwiOptions,
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

	// Handle build triggers: onChange/build-now materialize an owned ImageBuild
	// CR, which does the actual scheduling, polling and artifact recording.
	if buildErr := r.handleBuildTriggers(ctx, &ip); buildErr != nil {
		return r.fail(ctx, &ip, "BuildTriggerFailed", buildErr)
	}

	// Mirror the newest ImageBuild for this profile into status.lastBuild and
	// capture the saltboot boot image on success.
	requeue, mirrorErr := r.mirrorLatestBuild(ctx, uc, &ip)
	if mirrorErr != nil {
		// Non-fatal: log but continue so we don't lose the rest of the status.
		ctrl.LoggerFrom(ctx).Error(mirrorErr, "mirroring latest build status")
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

func (r *ImageProfileReconciler) handleDeletion(ctx context.Context, ip *uyuniv1.ImageProfile) (ctrl.Result, error) {
	if !containsFinalizer(ip, ipFinalizer) {
		return ctrl.Result{}, nil
	}
	if ip.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(ip, ipFinalizer)
		return ctrl.Result{}, r.Update(ctx, ip)
	}
	uc, err := r.Clients.ForOrganization(ctx, orgRef(ip.Spec.OrganizationRef), ip.Namespace)
	if err != nil {
		return ctrl.Result{}, err
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

// handleBuildTriggers materializes an owned ImageBuild CR when a build is
// requested (build-now annotation or onChange). The ImageBuild controller does
// the scheduling, polling and artifact recording; ImageProfile just declares
// intent. Names are deterministic (build-now keyed by version, onChange by spec
// generation) so a trigger creates exactly one ImageBuild and reruns are no-ops.
func (r *ImageProfileReconciler) handleBuildTriggers(ctx context.Context, ip *uyuniv1.ImageProfile) error {
	annBuildNow := ip.Annotations[uyuniv1.AnnBuildNow] == "true"
	onChange := ip.Spec.BuildPolicy == "onChange" &&
		(ip.Status.LastBuild == nil || ip.Status.ObservedGeneration < ip.Generation)

	if !annBuildNow && !onChange {
		return nil
	}

	// A build needs a build host. onChange without one is skipped silently; an
	// explicit build-now surfaces the misconfiguration. The ImageBuild controller
	// waits for the host to register, so we don't resolve it here.
	if ip.Spec.BuildHostRef == nil {
		if annBuildNow {
			return fmt.Errorf("spec.buildHostRef is required to trigger a build")
		}
		return nil
	}

	version := ip.Annotations[uyuniv1.AnnBuildVersion]

	trigger := "onChange"
	buildName := fmt.Sprintf("%s-gen%d", ip.Name, ip.Generation)
	if annBuildNow {
		trigger = "annotation"
		v := version
		if v == "" {
			v = time.Now().UTC().Format("20060102-1504")
		}
		buildName = fmt.Sprintf("%s-%s", ip.Name, v)
	}

	ib := &uyuniv1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:        buildName,
			Namespace:   ip.Namespace,
			Annotations: map[string]string{uyuniv1.AnnBuildTrigger: trigger},
		},
		Spec: uyuniv1.ImageBuildSpec{
			ProfileRef: uyuniv1.LocalObjectRef{Name: ip.Name},
			Version:    version, // empty => ImageBuild auto-generates a version tag
		},
	}
	if err := controllerutil.SetControllerReference(ip, ib, r.Scheme()); err != nil {
		return err
	}
	if err := r.Create(ctx, ib); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ImageBuild %q: %w", buildName, err)
	}
	ip.Status.LastBuildName = buildName

	if annBuildNow {
		patch := client.MergeFrom(ip.DeepCopy())
		delete(ip.Annotations, uyuniv1.AnnBuildNow)
		if err := r.Patch(ctx, ip, patch); err != nil {
			return fmt.Errorf("stripping build-now annotation: %w", err)
		}
	}
	return nil
}

// mirrorLatestBuild summarizes the newest ImageBuild referencing this profile
// into status.lastBuild, and on success reads the saltboot boot image from the
// image pillar (ImageProfile-specific data the ImageBuild does not record).
func (r *ImageProfileReconciler) mirrorLatestBuild(ctx context.Context, uc uyuni.API, ip *uyuniv1.ImageProfile) (time.Duration, error) {
	var builds uyuniv1.ImageBuildList
	if err := r.List(ctx, &builds, client.InNamespace(ip.Namespace)); err != nil {
		return 0, err
	}
	var latest *uyuniv1.ImageBuild
	for i := range builds.Items {
		b := &builds.Items[i]
		if b.Spec.ProfileRef.Name != ip.Name {
			continue
		}
		if latest == nil || b.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = b
		}
	}
	if latest == nil {
		return 0, nil
	}

	status := mapBuildStatus(latest.Status.BuildStatus)
	rec := &uyuniv1.ImageBuildRecord{
		BuildID:   latest.Status.ActionID,
		Version:   latest.Status.Version,
		Status:    status,
		Trigger:   latest.Annotations[uyuniv1.AnnBuildTrigger],
		Checksum:  latest.Status.Checksum,
		ImageURL:  latest.Status.ImageURL,
		StartedAt: latest.CreationTimestamp.DeepCopy(),
	}
	if latest.Status.ImageID != 0 {
		rec.BuildID = latest.Status.ImageID
	}
	if len(latest.Status.Files) > 0 {
		rec.Files = make([]uyuniv1.ImageFile, len(latest.Status.Files))
		copy(rec.Files, latest.Status.Files)
	}
	if status == "Succeeded" || status == "Failed" {
		if c := findCondition(latest.Status.Conditions, "Ready"); c != nil {
			t := c.LastTransitionTime
			rec.CompletedAt = &t
			if status == "Failed" {
				rec.FailureReason = c.Message
			}
		}
	}
	ip.Status.LastBuild = rec
	ip.Status.LastBuildName = latest.Name

	if status == "Succeeded" && latest.Status.ImageID != 0 {
		if imgs, err := uc.ListImages(ctx); err == nil {
			for _, img := range imgs {
				if img.ID == latest.Status.ImageID {
					if bi := r.bootImageFromPillar(ctx, uc, img); bi != "" {
						ip.Status.BootImage = bi
					}
					break
				}
			}
		}
	}

	if status == "Queued" || status == "Running" {
		return 30 * time.Second, nil
	}
	return 0, nil
}

// mapBuildStatus maps an ImageBuild's status.buildStatus to the ImageBuildRecord
// status vocabulary used on the ImageProfile.
func mapBuildStatus(s string) string {
	switch s {
	case "Succeeded":
		return "Succeeded"
	case "Failed":
		return "Failed"
	case "Running":
		return "Running"
	default: // "Scheduled" or empty
		return "Queued"
	}
}

// findCondition returns the condition of the given type, or nil.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// bootImageFromPillar reads the image pillar and returns the saltboot boot image
// identifier (e.g. "BranchServer_MicroOS-0.6.10-4") for a PXE/OS image, or "" if
// the image has no saltboot boot data (e.g. container images) or the pillar is
// unavailable.
func (r *ImageProfileReconciler) bootImageFromPillar(ctx context.Context, uc uyuni.API, img uyuni.ImageInfo) string {
	pillar, err := uc.GetImagePillar(ctx, img.ID)
	if err != nil {
		return ""
	}
	bootImages, ok := pillar["boot_images"].(map[string]any)
	if !ok {
		return ""
	}
	// boot_images is keyed by boot image name; this build's entry is
	// "<name>-<version>-<revision>".
	expected := fmt.Sprintf("%s-%s-%d", img.Name, img.Version, img.Revision)
	if _, ok := bootImages[expected]; ok {
		return expected
	}
	return ""
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
		Watches(&uyuniv1.ImageBuild{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForBuild)).
		Complete(r)
}

func (r *ImageProfileReconciler) profilesForBuild(ctx context.Context, obj client.Object) []reconcile.Request {
	ib, ok := obj.(*uyuniv1.ImageBuild)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: ib.Namespace, Name: ib.Spec.ProfileRef.Name},
	}}
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
