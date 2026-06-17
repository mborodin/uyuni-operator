package webhook

import (
	"context"
	"fmt"

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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-activationkey,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=create;update,versions=v1alpha1,name=vactivationkey.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ActivationKeyValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ActivationKeyValidator{}

func (v *ActivationKeyValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.ActivationKey{}).
		WithValidator(v).
		Complete()
}

func (v *ActivationKeyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.ActivationKey))
}

func (v *ActivationKeyValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldAK := oldObj.(*uyuniv1.ActivationKey)
	newAK := newObj.(*uyuniv1.ActivationKey)

	if oldAK.Spec.Key != newAK.Spec.Key {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: uyuniv1.Group, Resource: "activationkeys"},
			newAK.Name,
			fmt.Errorf("spec.key is immutable"))
	}
	return v.validate(ctx, newAK)
}

func (v *ActivationKeyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ActivationKeyValidator) validate(ctx context.Context, ak *uyuniv1.ActivationKey) (admission.Warnings, error) {
	errs := validation.ChannelRefMutex(
		ak.Spec.BaseChannelRef, ak.Spec.BaseChannelFrom,
		ak.Spec.ChildChannelRefs, ak.Spec.ChildChannelsFrom,
		field.NewPath("spec"))
	errs = append(errs, validation.StrictBooleanAnnotations(
		ak.Annotations, validation.DangerousAnnotations,
		field.NewPath("metadata", "annotations"))...)

	// Cross-resource: env declared in referenced project?
	var warnings admission.Warnings
	if ak.Spec.BaseChannelFrom != nil {
		w, ferr := v.validateProjectRef(ctx, ak.Namespace, ak.Spec.BaseChannelFrom, field.NewPath("spec", "baseChannelFrom"))
		if ferr != nil {
			errs = append(errs, ferr)
		} else if w != "" {
			warnings = append(warnings, w)
		}
	}
	for i, ref := range ak.Spec.ChildChannelsFrom {
		path := field.NewPath("spec", "childChannelsFrom").Index(i)
		w, ferr := v.validateProjectRef(ctx, ak.Namespace, &ref, path)
		if ferr != nil {
			errs = append(errs, ferr)
		} else if w != "" {
			warnings = append(warnings, w)
		}
	}

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "ActivationKey"},
			ak.Name, errs)
	}
	return warnings, nil
}

// validateProjectRef: hard error for typos (env not declared); warning
// only when the project itself is missing (Flux ordering tolerance).
func (v *ActivationKeyValidator) validateProjectRef(ctx context.Context, ns string, ref *uyuniv1.ChannelFromProject, path *field.Path) (string, *field.Error) {
	// An empty contentProjectRef means no content project channel is attached
	// — a valid state, not a misconfiguration. Nothing to validate; the
	// reconciler skips resolution for it too.
	if ref.ContentProjectRef.Name == "" {
		return "", nil
	}
	var cp uyuniv1.ContentProject
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.ContentProjectRef.Name}, &cp); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf(
				"%s: ContentProject %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), ref.ContentProjectRef.Name, ns), nil
		}
		// API server / network issue: don't block admission.
		return "", nil
	}
	for _, e := range cp.Spec.Environments {
		if e.Label == ref.Environment {
			return "", nil
		}
	}
	available := make([]string, 0, len(cp.Spec.Environments))
	for _, e := range cp.Spec.Environments {
		available = append(available, e.Label)
	}
	return "", field.Invalid(path.Child("environment"), ref.Environment,
		fmt.Sprintf("environment not declared in ContentProject %q (available: %v)", cp.Name, available))
}
