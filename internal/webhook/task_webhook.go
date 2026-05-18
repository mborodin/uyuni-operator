package webhook

import (
	"context"
	"fmt"
	"reflect"

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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-task,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=tasks,verbs=create;update,versions=v1alpha1,name=vtask.uyuni.uyuni-project.org,admissionReviewVersions=v1

type TaskValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &TaskValidator{}

func (v *TaskValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.Task{}).
		WithValidator(v).
		Complete()
}

func (v *TaskValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.Task))
}

func (v *TaskValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldT := oldObj.(*uyuniv1.Task)
	newT := newObj.(*uyuniv1.Task)

	// Post-execution immutability: once we have a run on record, the spec
	// is frozen except for ttlAfterFinished. Allowing changes after
	// execution would silently rewrite history.
	if len(oldT.Status.Runs) > 0 {
		old := oldT.Spec
		updated := newT.Spec
		old.TTLAfterFinished = updated.TTLAfterFinished
		if !reflect.DeepEqual(old, updated) {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: uyuniv1.Group, Resource: "tasks"},
				newT.Name,
				fmt.Errorf("spec is immutable after first execution (only ttlAfterFinished can change)"))
		}
	}
	return v.validate(ctx, newT)
}

func (v *TaskValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *TaskValidator) validate(ctx context.Context, t *uyuniv1.Task) (admission.Warnings, error) {
	errs := validation.TaskSpec(&t.Spec, field.NewPath("spec"))
	errs = append(errs, validation.StrictBooleanAnnotations(
		t.Annotations, validation.DangerousAnnotations,
		field.NewPath("metadata", "annotations"))...)

	// Cross-resource: target ref existence checks. Advisory.
	var warnings admission.Warnings
	if t.Spec.Target.SystemRef != nil {
		var sys uyuniv1.System
		if err := v.Client.Get(ctx, types.NamespacedName{
			Namespace: t.Namespace, Name: t.Spec.Target.SystemRef.Name,
		}, &sys); err != nil {
			if client.IgnoreNotFound(err) == nil {
				warnings = append(warnings,
					fmt.Sprintf("spec.target.systemRef: System %q not found in namespace %q",
						t.Spec.Target.SystemRef.Name, t.Namespace))
			}
		}
	}
	if t.Spec.Target.SystemGroupRef != nil {
		var sg uyuniv1.SystemGroup
		if err := v.Client.Get(ctx, types.NamespacedName{
			Namespace: t.Namespace, Name: t.Spec.Target.SystemGroupRef.Name,
		}, &sg); err != nil {
			if client.IgnoreNotFound(err) == nil {
				warnings = append(warnings,
					fmt.Sprintf("spec.target.systemGroupRef: SystemGroup %q not found in namespace %q",
						t.Spec.Target.SystemGroupRef.Name, t.Namespace))
			}
		}
	}

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "Task"},
			t.Name, errs)
	}
	return warnings, nil
}
