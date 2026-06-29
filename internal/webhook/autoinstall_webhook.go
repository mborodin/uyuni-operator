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
)

// =============================================================================
// AutoinstallDistribution
// =============================================================================

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-autoinstalldistribution,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=autoinstalldistributions,verbs=create;update,versions=v1alpha1,name=vautoinstalldistribution.uyuni.uyuni-project.org,admissionReviewVersions=v1

type AutoinstallDistributionValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &AutoinstallDistributionValidator{}

func (v *AutoinstallDistributionValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.AutoinstallDistribution{}).
		WithValidator(v).
		Complete()
}

func (v *AutoinstallDistributionValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateDistribution(ctx, obj.(*uyuniv1.AutoinstallDistribution))
}

func (v *AutoinstallDistributionValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.AutoinstallDistribution)
	ad := newObj.(*uyuniv1.AutoinstallDistribution)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "autoinstalldistributions"}

	if old.Spec.Label != ad.Spec.Label {
		return nil, apierrors.NewForbidden(gr, ad.Name, fmt.Errorf("spec.label is immutable"))
	}
	if old.Spec.InstallType != ad.Spec.InstallType {
		return nil, apierrors.NewForbidden(gr, ad.Name, fmt.Errorf("spec.installType is immutable"))
	}
	return v.validateDistribution(ctx, ad)
}

func (v *AutoinstallDistributionValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *AutoinstallDistributionValidator) validateDistribution(ctx context.Context, ad *uyuniv1.AutoinstallDistribution) (admission.Warnings, error) {
	var warnings admission.Warnings
	if w := v.warnIfChannelMissing(ctx, ad.Namespace, ad.Spec.ChannelRef.Name,
		field.NewPath("spec", "channelRef")); w != "" {
		warnings = append(warnings, w)
	}
	return warnings, nil
}

func (v *AutoinstallDistributionValidator) warnIfChannelMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sc uyuniv1.SoftwareChannel
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: SoftwareChannel %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

// =============================================================================
// AutoinstallProfile
// =============================================================================

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-autoinstallprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=autoinstallprofiles,verbs=create;update,versions=v1alpha1,name=vautoinstallprofile.uyuni.uyuni-project.org,admissionReviewVersions=v1

type AutoinstallProfileValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &AutoinstallProfileValidator{}

func (v *AutoinstallProfileValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.AutoinstallProfile{}).
		WithValidator(v).
		Complete()
}

func (v *AutoinstallProfileValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateProfile(ctx, obj.(*uyuniv1.AutoinstallProfile))
}

func (v *AutoinstallProfileValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.AutoinstallProfile)
	ap := newObj.(*uyuniv1.AutoinstallProfile)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "autoinstallprofiles"}

	if old.Spec.Label != ap.Spec.Label {
		return nil, apierrors.NewForbidden(gr, ap.Name, fmt.Errorf("spec.label is immutable"))
	}
	if old.Spec.Mode != ap.Spec.Mode {
		return nil, apierrors.NewForbidden(gr, ap.Name, fmt.Errorf("spec.mode is immutable"))
	}
	oldDist, newDist := "", ""
	if old.Spec.DistributionRef != nil {
		oldDist = old.Spec.DistributionRef.Name
	}
	if ap.Spec.DistributionRef != nil {
		newDist = ap.Spec.DistributionRef.Name
	}
	if oldDist != newDist {
		return nil, apierrors.NewForbidden(gr, ap.Name, fmt.Errorf("spec.distributionRef is immutable"))
	}
	return v.validateProfile(ctx, ap)
}

func (v *AutoinstallProfileValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *AutoinstallProfileValidator) validateProfile(ctx context.Context, ap *uyuniv1.AutoinstallProfile) (admission.Warnings, error) {
	var errs field.ErrorList

	// KickstartContents and Scripts are mutually exclusive.
	if ap.Spec.KickstartContents != "" && len(ap.Spec.Scripts) > 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "scripts"), ap.Spec.Scripts,
			"spec.scripts must be empty when spec.kickstartContents is set (the imported file is authoritative)"))
	}

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "AutoinstallProfile"},
			ap.Name, errs)
	}

	var warnings admission.Warnings

	// Cross-resource: warn if referenced AutoinstallDistribution not found
	// (Managed mode only; External profiles reference a Cobbler-only tree).
	if ap.Spec.DistributionRef != nil {
		if w := v.warnIfDistributionMissing(ctx, ap.Namespace, ap.Spec.DistributionRef.Name,
			field.NewPath("spec", "distributionRef")); w != "" {
			warnings = append(warnings, w)
		}
	}

	// Cross-resource: warn if any child channel not found.
	for i, ref := range ap.Spec.ChildChannelRefs {
		path := field.NewPath("spec", "childChannelRefs").Index(i)
		if w := v.warnIfSoftwareChannelMissing(ctx, ap.Namespace, ref.Name, path); w != "" {
			warnings = append(warnings, w)
		}
	}

	// Advisory warning: kickstartContents + variables is allowed but variables won't be
	// substituted into an imported file the same way as a managed profile.
	if ap.Spec.KickstartContents != "" && len(ap.Spec.Variables) > 0 {
		warnings = append(warnings,
			"spec.variables are set alongside spec.kickstartContents; variables are not substituted into imported files by Uyuni")
	}

	return warnings, nil
}

func (v *AutoinstallProfileValidator) warnIfDistributionMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var ad uyuniv1.AutoinstallDistribution
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ad); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: AutoinstallDistribution %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}

func (v *AutoinstallProfileValidator) warnIfSoftwareChannelMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sc uyuniv1.SoftwareChannel
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: SoftwareChannel %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
	}
	return ""
}
