package webhook

import (
	"context"
	"fmt"
	"strings"

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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-maintenanceschedule,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=maintenanceschedules,verbs=create;update,versions=v1alpha1,name=vmaintenanceschedule.uyuni.uyuni-project.org,admissionReviewVersions=v1

type MaintenanceScheduleValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &MaintenanceScheduleValidator{}

func (v *MaintenanceScheduleValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.MaintenanceSchedule{}).
		WithValidator(v).
		Complete()
}

func (v *MaintenanceScheduleValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.MaintenanceSchedule))
}

func (v *MaintenanceScheduleValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldMS := oldObj.(*uyuniv1.MaintenanceSchedule)
	newMS := newObj.(*uyuniv1.MaintenanceSchedule)
	// The Uyuni schedule name is the identity of the schedule and cannot be
	// renamed; changing it would orphan the Uyuni-side schedule. spec.type
	// is deliberately NOT checked here — Uyuni's updateSchedule accepts a
	// new type, and MaintenanceScheduleTargetCount (called from validate,
	// below) already rejects a Multi->Single change that still lists
	// multiple targets.
	if oldMS.Spec.Name != newMS.Spec.Name {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "MaintenanceSchedule"},
			newMS.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "name"), "name is immutable")})
	}
	return v.validate(ctx, newMS)
}

func (v *MaintenanceScheduleValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *MaintenanceScheduleValidator) validate(ctx context.Context, ms *uyuniv1.MaintenanceSchedule) (admission.Warnings, error) {
	errs := validation.MaintenanceScheduleTargetCount(
		ms.Spec.Type, ms.Spec.SystemRefs, ms.Spec.SystemGroupRefs, field.NewPath("spec"))

	var warnings admission.Warnings

	// Cross-resource: warn (GitOps tolerance) if the referenced calendar
	// isn't found yet — it may be applied alongside in the same commit.
	if ms.Spec.CalendarRef != nil {
		w := v.warnIfCalendarMissing(ctx, ms.Namespace, ms.Spec.CalendarRef.Name,
			field.NewPath("spec", "calendarRef"))
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	for i, ref := range ms.Spec.SystemRefs {
		w := v.warnIfSystemMissing(ctx, ms.Namespace, ref.Name,
			field.NewPath("spec", "systemRefs").Index(i))
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	for i, ref := range ms.Spec.SystemGroupRefs {
		w := v.warnIfSystemGroupMissing(ctx, ms.Namespace, ref.Name,
			field.NewPath("spec", "systemGroupRefs").Index(i))
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "MaintenanceSchedule"},
			ms.Name, errs)
	}
	return warnings, nil
}

func (v *MaintenanceScheduleValidator) warnIfCalendarMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var mc uyuniv1.MaintenanceCalendar
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &mc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: MaintenanceCalendar %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
		// API server / network issue: don't block admission.
		return ""
	}
	return ""
}

// warnIfSystemMissing mirrors the controller's findSystem resolution chain
// (exact name, spec.minionId, spec.hostname — deliberately no k8s-name
// suffix match, see findSystem's doc comment) so a validly short-named ref
// doesn't produce a false "not found" warning here.
func (v *MaintenanceScheduleValidator) warnIfSystemMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sys uyuniv1.System
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sys); err == nil {
		return ""
	} else if client.IgnoreNotFound(err) != nil {
		return ""
	}

	var list uyuniv1.SystemList
	if err := v.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return ""
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.MinionID == name || s.Spec.Hostname == name {
			return ""
		}
		if short, _, ok := strings.Cut(s.Spec.Hostname, "."); ok && short == name {
			return ""
		}
	}
	return fmt.Sprintf("%s: System %q not found in namespace %q (may be applied alongside in the same commit)",
		path.String(), name, ns)
}

func (v *MaintenanceScheduleValidator) warnIfSystemGroupMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sg uyuniv1.SystemGroup
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sg); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: SystemGroup %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
		return ""
	}
	return ""
}
