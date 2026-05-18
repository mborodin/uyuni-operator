package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-organization,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=organizations,verbs=create;update,versions=v1alpha1,name=vorganization.uyuni.uyuni-project.org,admissionReviewVersions=v1

type OrganizationValidator struct{}

var _ webhook.CustomValidator = &OrganizationValidator{}

func (v *OrganizationValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.Organization{}).
		WithValidator(v).
		Complete()
}

func (v *OrganizationValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	org := obj.(*uyuniv1.Organization)
	if err := v.validateSpec(org, nil); err != nil {
		return nil, err
	}
	return nil, nil
}

func (v *OrganizationValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.Organization)
	org := newObj.(*uyuniv1.Organization)
	if err := v.validateSpec(org, old); err != nil {
		return nil, err
	}
	return nil, nil
}

func (v *OrganizationValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *OrganizationValidator) validateSpec(org *uyuniv1.Organization, old *uyuniv1.Organization) error {
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "organizations"}

	if org.Spec.Name == "" {
		return apierrors.NewForbidden(gr, org.Name, fmt.Errorf("spec.name is required"))
	}
	if org.Spec.ProviderRef.Name == "" {
		return apierrors.NewForbidden(gr, org.Name, fmt.Errorf("spec.providerRef.name is required"))
	}

	// Immutability guards (update only).
	if old != nil {
		if old.Spec.Name != "" && org.Spec.Name != old.Spec.Name {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.name is immutable after creation"))
		}
		if old.Spec.ProviderRef.Name != "" && org.Spec.ProviderRef.Name != old.Spec.ProviderRef.Name {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.providerRef.name is immutable after creation"))
		}
		oldImportID := 0
		if old.Spec.Import != nil {
			oldImportID = old.Spec.Import.OrganizationID
		}
		newImportID := 0
		if org.Spec.Import != nil {
			newImportID = org.Spec.Import.OrganizationID
		}
		if oldImportID != 0 && newImportID != oldImportID {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.import.organizationId is immutable once set"))
		}
	}

	return nil
}
