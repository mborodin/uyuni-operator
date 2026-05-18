package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-uyuniprovider,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=uyuniproviders,verbs=create;update,versions=v1alpha1,name=vuyuniprovider.uyuni.uyuni-project.org,admissionReviewVersions=v1

type UyuniProviderValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &UyuniProviderValidator{}

func (v *UyuniProviderValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.UyuniProvider{}).
		WithValidator(v).
		Complete()
}

func (v *UyuniProviderValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.UyuniProvider))
}

func (v *UyuniProviderValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj.(*uyuniv1.UyuniProvider))
}

func (v *UyuniProviderValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *UyuniProviderValidator) validate(ctx context.Context, prov *uyuniv1.UyuniProvider) (admission.Warnings, error) {
	if !prov.Spec.IsDefault {
		return nil, nil
	}
	var list uyuniv1.UyuniProviderList
	if err := v.Client.List(ctx, &list); err != nil {
		return admission.Warnings{
			"could not verify uniqueness of isDefault; will be re-validated at reconcile",
		}, nil
	}
	for _, other := range list.Items {
		if other.Name == prov.Name {
			continue
		}
		if other.Spec.IsDefault {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: uyuniv1.Group, Resource: "uyuniproviders"},
				prov.Name,
				fmt.Errorf("UyuniProvider %q is already marked isDefault=true; only one default is permitted",
					other.Name))
		}
	}
	return nil, nil
}
