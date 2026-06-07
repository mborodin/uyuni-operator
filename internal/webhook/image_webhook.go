package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/validation"
)

// =============================================================================
// ImageStore
// =============================================================================

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-imagestore,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=imagestores,verbs=create;update,versions=v1alpha1,name=vimagestore.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ImageStoreValidator struct{}

var _ webhook.CustomValidator = &ImageStoreValidator{}

func (v *ImageStoreValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ImageStore{}).
		WithValidator(v).
		Complete()
}

func (v *ImageStoreValidator) ValidateCreate(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ImageStoreValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.ImageStore)
	is := newObj.(*uyuniv1.ImageStore)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "imagestores"}
	if old.Spec.Label != is.Spec.Label {
		return nil, apierrors.NewForbidden(gr, is.Name, fmt.Errorf("spec.label is immutable"))
	}
	if old.Spec.Type != is.Spec.Type {
		return nil, apierrors.NewForbidden(gr, is.Name, fmt.Errorf("spec.type is immutable"))
	}
	return nil, nil
}

func (v *ImageStoreValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// =============================================================================
// ImageProfile
// =============================================================================

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-imageprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=imageprofiles,verbs=create;update,versions=v1alpha1,name=vimageprofile.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ImageProfileValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ImageProfileValidator{}

func (v *ImageProfileValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ImageProfile{}).
		WithValidator(v).
		Complete()
}

func (v *ImageProfileValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateProfile(ctx, obj.(*uyuniv1.ImageProfile))
}

func (v *ImageProfileValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.ImageProfile)
	ip := newObj.(*uyuniv1.ImageProfile)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "imageprofiles"}
	if old.Spec.Label != ip.Spec.Label {
		return nil, apierrors.NewForbidden(gr, ip.Name, fmt.Errorf("spec.label is immutable"))
	}
	if old.Spec.Type != ip.Spec.Type {
		return nil, apierrors.NewForbidden(gr, ip.Name, fmt.Errorf("spec.type is immutable"))
	}
	return v.validateProfile(ctx, ip)
}

func (v *ImageProfileValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ImageProfileValidator) validateProfile(ctx context.Context, ip *uyuniv1.ImageProfile) (admission.Warnings, error) {
	var errs field.ErrorList

	// Exactly one of url or git must be set.
	if ip.Spec.URL != "" && ip.Spec.Git != nil {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "git"), ip.Spec.Git,
			"spec.url and spec.git are mutually exclusive; set exactly one"))
	} else if ip.Spec.URL == "" && ip.Spec.Git == nil {
		errs = append(errs, field.Required(
			field.NewPath("spec"),
			"exactly one of spec.url or spec.git must be set"))
	}

	// git.path without git.reference is ambiguous.
	if ip.Spec.Git != nil && ip.Spec.Git.Path != "" && ip.Spec.Git.Reference == "" {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "git", "path"), ip.Spec.Git.Path,
			"spec.git.path requires spec.git.reference to be set"))
	}

	errs = append(errs, validation.StrictBooleanAnnotations(
		ip.Annotations, []string{uyuniv1.AnnBuildNow, uyuniv1.AnnForceDelete},
		field.NewPath("metadata", "annotations"))...)

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "ImageProfile"},
			ip.Name, errs)
	}

	var warnings admission.Warnings

	// Cross-resource: warn if storeRef not found or not yet realized.
	if w := v.warnIfStoreMissing(ctx, ip.Namespace, ip.Spec.StoreRef.Name,
		field.NewPath("spec", "storeRef")); w != "" {
		warnings = append(warnings, w)
	}

	// Cross-resource: warn if activationKeyRef not found.
	if ip.Spec.ActivationKeyRef != nil {
		if w := v.warnIfActivationKeyMissing(ctx, ip.Namespace, ip.Spec.ActivationKeyRef.Name,
			field.NewPath("spec", "activationKeyRef")); w != "" {
			warnings = append(warnings, w)
		}
	}

	// Cross-resource: warn if buildHostRef not found.
	if ip.Spec.BuildHostRef != nil {
		if w := v.warnIfSystemMissing(ctx, ip.Namespace, ip.Spec.BuildHostRef.Name,
			field.NewPath("spec", "buildHostRef")); w != "" {
			warnings = append(warnings, w)
		}
	}

	return warnings, nil
}

func (v *ImageProfileValidator) warnIfStoreMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var is uyuniv1.ImageStore
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &is); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: ImageStore %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

func (v *ImageProfileValidator) warnIfActivationKeyMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var ak uyuniv1.ActivationKey
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ak); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: ActivationKey %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

func (v *ImageProfileValidator) warnIfSystemMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sys uyuniv1.System
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sys); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: System %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

// =============================================================================
// ImageBuild
// =============================================================================

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-imagebuild,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=imagebuilds,verbs=create;update,versions=v1alpha1,name=vimagebuild.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ImageBuildValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ImageBuildValidator{}

func (v *ImageBuildValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ImageBuild{}).
		WithValidator(v).
		Complete()
}

func (v *ImageBuildValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateBuild(ctx, obj.(*uyuniv1.ImageBuild))
}

func (v *ImageBuildValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.ImageBuild)
	ib := newObj.(*uyuniv1.ImageBuild)

	// profileRef is immutable once a build has been scheduled.
	if old.Status.ActionID != 0 && old.Spec.ProfileRef.Name != ib.Spec.ProfileRef.Name {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: uyuniv1.Group, Resource: "imagebuilds"},
			ib.Name,
			fmt.Errorf("spec.profileRef is immutable after a build has been scheduled"))
	}

	return v.validateBuild(ctx, ib)
}

func (v *ImageBuildValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ImageBuildValidator) validateBuild(ctx context.Context, ib *uyuniv1.ImageBuild) (admission.Warnings, error) {
	errs := validation.StrictBooleanAnnotations(
		ib.Annotations, []string{uyuniv1.AnnBuildNow, uyuniv1.AnnForceDelete},
		field.NewPath("metadata", "annotations"))

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "ImageBuild"},
			ib.Name, errs)
	}

	var warnings admission.Warnings

	// Cross-resource: warn if profileRef not found.
	if w := v.warnIfProfileMissing(ctx, ib.Namespace, ib.Spec.ProfileRef.Name,
		field.NewPath("spec", "profileRef")); w != "" {
		warnings = append(warnings, w)
	}

	// Cross-resource: warn if buildHostRef not found.
	if ib.Spec.BuildHostRef != nil {
		if w := v.warnIfSystemMissing(ctx, ib.Namespace, ib.Spec.BuildHostRef.Name,
			field.NewPath("spec", "buildHostRef")); w != "" {
			warnings = append(warnings, w)
		}
	}

	return warnings, nil
}

func (v *ImageBuildValidator) warnIfProfileMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var ip uyuniv1.ImageProfile
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ip); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: ImageProfile %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

func (v *ImageBuildValidator) warnIfSystemMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sys uyuniv1.System
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sys); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: System %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}
