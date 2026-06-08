package webhook

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// autoinstall.profile and autoinstall.profileRef are immutable once the first
	// provisioning action has been scheduled (status.autoinstallActionId is non-zero).
	if oldSys.Spec.Autoinstall != nil && newSys.Spec.Autoinstall != nil &&
		oldSys.Status.AutoinstallActionID != 0 {
		if oldSys.Spec.Autoinstall.Profile != newSys.Spec.Autoinstall.Profile {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: uyuniv1.Group, Resource: "systems"},
				newSys.Name,
				fmt.Errorf("spec.autoinstall.profile is immutable after provisioning has been scheduled"))
		}
		oldRef := ""
		if oldSys.Spec.Autoinstall.ProfileRef != nil {
			oldRef = oldSys.Spec.Autoinstall.ProfileRef.Name
		}
		newRef := ""
		if newSys.Spec.Autoinstall.ProfileRef != nil {
			newRef = newSys.Spec.Autoinstall.ProfileRef.Name
		}
		if oldRef != newRef {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: uyuniv1.Group, Resource: "systems"},
				newSys.Name,
				fmt.Errorf("spec.autoinstall.profileRef is immutable after provisioning has been scheduled"))
		}
	}

	return v.validate(ctx, newSys)
}

func (v *SystemValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *SystemValidator) validate(ctx context.Context, sys *uyuniv1.System) (admission.Warnings, error) {
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

	// reinstall-now requires spec.autoinstall to be set.
	if sys.Annotations[uyuniv1.AnnReinstallNow] == "true" && sys.Spec.Autoinstall == nil {
		errs = append(errs, field.Required(
			field.NewPath("spec", "autoinstall"),
			"spec.autoinstall must be set when using the reinstall-now annotation"))
	}

	// spec.autoinstall.profile and spec.autoinstall.profileRef are mutually exclusive.
	if sys.Spec.Autoinstall != nil &&
		sys.Spec.Autoinstall.Profile != "" && sys.Spec.Autoinstall.ProfileRef != nil {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "autoinstall", "profileRef"),
			sys.Spec.Autoinstall.ProfileRef,
			"spec.autoinstall.profile and spec.autoinstall.profileRef are mutually exclusive; set exactly one"))
	}

	var warnings admission.Warnings

	// Cross-resource: warn (GitOps tolerance) if referenced groups are not found.
	for i, ref := range sys.Spec.GroupRefs {
		path := field.NewPath("spec", "groupRefs").Index(i)
		w := v.warnIfGroupMissing(ctx, sys.Namespace, ref.Name, path)
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	// Cross-resource: warn if directly referenced config channels are not found.
	for i, ref := range sys.Spec.ConfigChannelRefs {
		path := field.NewPath("spec", "configChannelRefs").Index(i)
		w := v.warnIfConfigChannelMissing(ctx, sys.Namespace, ref.Name, path)
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	// Cross-resource: warn if spec.autoinstall.profileRef does not resolve.
	if sys.Spec.Autoinstall != nil && sys.Spec.Autoinstall.ProfileRef != nil {
		path := field.NewPath("spec", "autoinstall", "profileRef")
		w := v.warnIfAutoinstallProfileMissing(ctx, sys.Namespace, sys.Spec.Autoinstall.ProfileRef.Name, path)
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "System"},
			sys.Name, errs)
	}
	return warnings, nil
}

func (v *SystemValidator) warnIfGroupMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var sg uyuniv1.SystemGroup
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sg); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: SystemGroup %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
		// API server / network issue: don't block admission.
		return ""
	}
	return ""
}

func (v *SystemValidator) warnIfConfigChannelMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var cc uyuniv1.ConfigurationChannel
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &cc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: ConfigurationChannel %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
		return ""
	}
	return ""
}

func (v *SystemValidator) warnIfAutoinstallProfileMissing(ctx context.Context, ns, name string, path *field.Path) string {
	var ap uyuniv1.AutoinstallProfile
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ap); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("%s: AutoinstallProfile %q not found in namespace %q (may be applied alongside in the same commit)",
				path.String(), name, ns)
		}
		return ""
	}
	return ""
}

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-systemgroup,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=systemgroups,verbs=create;update,versions=v1alpha1,name=vsystemgroup.uyuni.uyuni-project.org,admissionReviewVersions=v1

type SystemGroupValidator struct{}

var _ webhook.CustomValidator = &SystemGroupValidator{}

func (v *SystemGroupValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.SystemGroup{}).
		WithValidator(v).
		Complete()
}

func (v *SystemGroupValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *SystemGroupValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.SystemGroup)
	sg := newObj.(*uyuniv1.SystemGroup)
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "systemgroups"}

	if old.Spec.Name != sg.Spec.Name {
		return nil, apierrors.NewForbidden(gr, sg.Name, fmt.Errorf("spec.name is immutable"))
	}
	if !reflect.DeepEqual(old.Spec.Cluster, sg.Spec.Cluster) {
		return nil, apierrors.NewForbidden(gr, sg.Name, fmt.Errorf("spec.cluster is immutable"))
	}
	return nil, nil
}

func (v *SystemGroupValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
