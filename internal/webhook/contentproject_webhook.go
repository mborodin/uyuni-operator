package webhook

import (
	"context"
	"fmt"
	"reflect"

	"github.com/robfig/cron/v3"
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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-contentproject,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=create;update,versions=v1alpha1,name=vcontentproject.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ContentProjectValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ContentProjectValidator{}

func (v *ContentProjectValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ContentProject{}).
		WithValidator(v).
		Complete()
}

func (v *ContentProjectValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.ContentProject))
}

func (v *ContentProjectValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldCP := oldObj.(*uyuniv1.ContentProject)
	newCP := newObj.(*uyuniv1.ContentProject)
	if oldCP.Spec.Label != newCP.Spec.Label {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: uyuniv1.Group, Resource: "contentprojects"},
			newCP.Name,
			fmt.Errorf("spec.label is immutable"))
	}
	return v.validate(ctx, newCP)
}

func (v *ContentProjectValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ContentProjectValidator) validate(_ context.Context, cp *uyuniv1.ContentProject) (admission.Warnings, error) {
	var errs field.ErrorList
	// Only validate environments if specified. Environments are now managed via separate ClmEnvironment CRDs.
	if len(cp.Spec.Environments) > 0 {
		errs = validation.EnvChain(cp.Spec.Environments, field.NewPath("spec", "environments"))
	}

	if cp.Spec.Build.Schedule != "" {
		if _, err := cron.ParseStandard(cp.Spec.Build.Schedule); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "build", "schedule"),
				cp.Spec.Build.Schedule,
				fmt.Sprintf("invalid cron expression: %s", err)))
		}
	}

	seenFilter := map[string]bool{}
	for i, f := range cp.Spec.Filters {
		if seenFilter[f.Name] {
			errs = append(errs, field.Duplicate(
				field.NewPath("spec", "filters").Index(i).Child("name"), f.Name))
		}
		seenFilter[f.Name] = true
	}

	errs = append(errs, validation.StrictBooleanAnnotations(
		cp.Annotations, validation.DangerousAnnotations,
		field.NewPath("metadata", "annotations"))...)

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "ContentProject"},
			cp.Name, errs)
	}
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-contentprojectpromotion,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=contentprojectpromotions,verbs=create;update,versions=v1alpha1,name=vpromotion.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ContentProjectPromotionValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ContentProjectPromotionValidator{}

func (v *ContentProjectPromotionValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ContentProjectPromotion{}).
		WithValidator(v).
		Complete()
}

func (v *ContentProjectPromotionValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.ContentProjectPromotion))
}

func (v *ContentProjectPromotionValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldP := oldObj.(*uyuniv1.ContentProjectPromotion)
	newP := newObj.(*uyuniv1.ContentProjectPromotion)

	// Spec frozen past Pending. ttlAfterFinished is the one mutable field
	// (customer can extend retention).
	if oldP.Status.Phase != "" && oldP.Status.Phase != "Pending" {
		old := oldP.Spec
		updated := newP.Spec
		old.TTLAfterFinished = updated.TTLAfterFinished
		if !reflect.DeepEqual(old, updated) {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: uyuniv1.Group, Resource: "contentprojectpromotions"},
				newP.Name,
				fmt.Errorf("spec is immutable once promotion is past Pending (only ttlAfterFinished can change)"))
		}
		return nil, nil
	}
	return v.validate(ctx, newP)
}

func (v *ContentProjectPromotionValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ContentProjectPromotionValidator) validate(ctx context.Context, p *uyuniv1.ContentProjectPromotion) (admission.Warnings, error) {
	var errs field.ErrorList
	var warnings admission.Warnings

	var cp uyuniv1.ContentProject
	if err := v.Client.Get(ctx, types.NamespacedName{
		Namespace: p.Namespace, Name: p.Spec.ProjectRef.Name,
	}, &cp); err != nil {
		if client.IgnoreNotFound(err) == nil {
			warnings = append(warnings, fmt.Sprintf(
				"ContentProject %q not found; will be re-validated at reconcile", p.Spec.ProjectRef.Name))
			return warnings, nil
		}
		return nil, nil
	}

	errs = append(errs,
		validation.PromotionPair(&cp,
			p.Spec.FromEnvironment, p.Spec.ToEnvironment,
			field.NewPath("spec", "fromEnvironment"),
			field.NewPath("spec", "toEnvironment"))...)

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "ContentProjectPromotion"},
			p.Name, errs)
	}
	return warnings, nil
}
