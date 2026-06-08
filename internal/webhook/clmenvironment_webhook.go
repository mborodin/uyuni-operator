package webhook

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-clmenvironment,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=clmenvironments,verbs=create;update,versions=v1alpha1,name=vclmenvironment.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ClmEnvironmentValidator struct{}

var _ webhook.CustomValidator = &ClmEnvironmentValidator{}

func (v *ClmEnvironmentValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ClmEnvironment{}).
		WithValidator(v).
		Complete()
}

func (v *ClmEnvironmentValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClmEnvironmentValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.ClmEnvironment)
	env := newObj.(*uyuniv1.ClmEnvironment)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "clmenvironments"}

	// Immutable fields
	if old.Spec.Id != env.Spec.Id {
		return nil, apierrors.NewForbidden(gr, env.Name, fmt.Errorf("spec.id is immutable"))
	}
	if old.Spec.ProjectRef.Name != env.Spec.ProjectRef.Name {
		return nil, apierrors.NewForbidden(gr, env.Name, fmt.Errorf("spec.projectRef is immutable"))
	}
	if !reflect.DeepEqual(old.Spec.Cluster, env.Spec.Cluster) {
		return nil, apierrors.NewForbidden(gr, env.Name, fmt.Errorf("spec.cluster is immutable"))
	}
	return nil, nil
}

func (v *ClmEnvironmentValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
