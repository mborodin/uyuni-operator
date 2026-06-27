package webhook

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-custominfokey,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=custominfokeys,verbs=create;update,versions=v1alpha1,name=vcustominfokey.uyuni.uyuni-project.org,admissionReviewVersions=v1

type CustomInfoKeyValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &CustomInfoKeyValidator{}

func (v *CustomInfoKeyValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.CustomInfoKey{}).
		WithValidator(v).
		Complete()
}

func (v *CustomInfoKeyValidator) ValidateCreate(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *CustomInfoKeyValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldKey := oldObj.(*uyuniv1.CustomInfoKey)
	newKey := newObj.(*uyuniv1.CustomInfoKey)
	// The Uyuni custom info key label is the identity of the key and cannot be
	// renamed; changing it would orphan the Uyuni-side key.
	if oldKey.Spec.Label != newKey.Spec.Label {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "CustomInfoKey"},
			newKey.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "label"), "label is immutable")})
	}
	return nil, nil
}

func (v *CustomInfoKeyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
