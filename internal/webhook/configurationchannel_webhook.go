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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-configurationchannel,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=configurationchannels,verbs=create;update,versions=v1alpha1,name=vconfigurationchannel.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ConfigurationChannelValidator struct{}

var _ webhook.CustomValidator = &ConfigurationChannelValidator{}

func (v *ConfigurationChannelValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ConfigurationChannel{}).
		WithValidator(v).
		Complete()
}

func (v *ConfigurationChannelValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj.(*uyuniv1.ConfigurationChannel))
}

func (v *ConfigurationChannelValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.ConfigurationChannel)
	cc := newObj.(*uyuniv1.ConfigurationChannel)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "configurationchannels"}

	if old.Spec.ID != cc.Spec.ID {
		return nil, apierrors.NewForbidden(gr, cc.Name, fmt.Errorf("spec.id is immutable"))
	}
	if old.Spec.Type != cc.Spec.Type {
		return nil, apierrors.NewForbidden(gr, cc.Name, fmt.Errorf("spec.type is immutable"))
	}
	if old.Spec.Cluster != cc.Spec.Cluster {
		return nil, apierrors.NewForbidden(gr, cc.Name, fmt.Errorf("spec.cluster is immutable"))
	}
	return v.validate(cc)
}

func (v *ConfigurationChannelValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ConfigurationChannelValidator) validate(cc *uyuniv1.ConfigurationChannel) (admission.Warnings, error) {
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "configurationchannels"}
	if cc.Spec.ID == "" {
		return nil, apierrors.NewForbidden(gr, cc.Name, fmt.Errorf("spec.id is required"))
	}
	if cc.Spec.Name == "" {
		return nil, apierrors.NewForbidden(gr, cc.Name, fmt.Errorf("spec.name is required"))
	}
	return nil, nil
}
