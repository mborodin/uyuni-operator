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
	"github.com/mborodin/uyuni-operator/internal/validation"
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-maintenancecalendar,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=maintenancecalendars,verbs=create;update,versions=v1alpha1,name=vmaintenancecalendar.uyuni.uyuni-project.org,admissionReviewVersions=v1

type MaintenanceCalendarValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &MaintenanceCalendarValidator{}

func (v *MaintenanceCalendarValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.MaintenanceCalendar{}).
		WithValidator(v).
		Complete()
}

func (v *MaintenanceCalendarValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj.(*uyuniv1.MaintenanceCalendar))
}

func (v *MaintenanceCalendarValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldMC := oldObj.(*uyuniv1.MaintenanceCalendar)
	newMC := newObj.(*uyuniv1.MaintenanceCalendar)
	// The Uyuni calendar label is the identity of the calendar and cannot be
	// renamed; changing it would orphan the Uyuni-side calendar.
	if oldMC.Spec.Label != newMC.Spec.Label {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "MaintenanceCalendar"},
			newMC.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "label"), "label is immutable")})
	}
	return v.validate(newMC)
}

func (v *MaintenanceCalendarValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *MaintenanceCalendarValidator) validate(mc *uyuniv1.MaintenanceCalendar) (admission.Warnings, error) {
	errs := validation.MaintenanceCalendarSourceMutex(
		mc.Spec.ICal, mc.Spec.URL, field.NewPath("spec"))

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "MaintenanceCalendar"},
			mc.Name, errs)
	}
	return nil, nil
}
