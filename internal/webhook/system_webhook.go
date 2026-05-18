package webhook

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/validation"
)

// +kubebuilder:webhook:path=/mutate-uyuni-uyuni-project-org-v1alpha1-system,mutating=true,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=systems,verbs=create;update,versions=v1alpha1,name=msystem.uyuni.uyuni-project.org,admissionReviewVersions=v1

type SystemDefaulter struct{}

var _ webhook.CustomDefaulter = &SystemDefaulter{}

func (d *SystemDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.System{}).
		WithDefaulter(d).
		Complete()
}

func (d *SystemDefaulter) Default(_ context.Context, obj runtime.Object) error {
	sys := obj.(*uyuniv1.System)

	if sys.Spec.Hostname == "" && sys.Spec.MinionID != "" {
		sys.Spec.Hostname = sys.Spec.MinionID
	}

	if sys.Spec.AdoptionTimeout.Duration == 0 {
		if sys.Spec.PreCreate {
			sys.Spec.AdoptionTimeout = metav1.Duration{Duration: 24 * time.Hour}
		} else {
			sys.Spec.AdoptionTimeout = metav1.Duration{Duration: 30 * time.Minute}
		}
	}

	if sys.Spec.DeletionPolicy == "" {
		sys.Spec.DeletionPolicy = "Orphan"
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-system,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=systems,verbs=create;update,versions=v1alpha1,name=vsystem.uyuni.uyuni-project.org,admissionReviewVersions=v1

type SystemValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &SystemValidator{}

func (v *SystemValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.System{}).
		WithValidator(v).
		Complete()
}

func (v *SystemValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.System))
}

func (v *SystemValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldSys := oldObj.(*uyuniv1.System)
	newSys := newObj.(*uyuniv1.System)

	if oldSys.Spec.MinionID != newSys.Spec.MinionID {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: uyuniv1.Group, Resource: "systems"},
			newSys.Name,
			fmt.Errorf("spec.minionId is immutable"))
	}
	return v.validate(ctx, newSys)
}

func (v *SystemValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *SystemValidator) validate(_ context.Context, sys *uyuniv1.System) (admission.Warnings, error) {
	errs := validation.ChannelRefMutex(
		sys.Spec.BaseChannelRef, sys.Spec.BaseChannelFrom,
		sys.Spec.ChildChannelRefs, sys.Spec.ChildChannelsFrom,
		field.NewPath("spec"))
	errs = append(errs, validation.PreCreateRequiresIdentification(
		sys.Spec.PreCreate, sys.Spec.Hostname, sys.Spec.Network,
		field.NewPath("spec"))...)
	errs = append(errs, validation.StrictBooleanAnnotations(
		sys.Annotations, validation.DangerousAnnotations,
		field.NewPath("metadata", "annotations"))...)

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "System"},
			sys.Name, errs)
	}
	return nil, nil
}
