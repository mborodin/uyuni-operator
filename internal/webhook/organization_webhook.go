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

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-organization,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=organizations,verbs=create;update,versions=v1alpha1,name=vorganization.uyuni.uyuni-project.org,admissionReviewVersions=v1

type OrganizationValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &OrganizationValidator{}

func (v *OrganizationValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.Organization{}).
		WithValidator(v).
		Complete()
}

func (v *OrganizationValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	org := obj.(*uyuniv1.Organization)
	if err := v.validateSpec(org, nil); err != nil {
		return nil, err
	}
	return v.checkDuplicate(ctx, org)
}

func (v *OrganizationValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old := oldObj.(*uyuniv1.Organization)
	org := newObj.(*uyuniv1.Organization)
	// checkDuplicate deliberately does NOT run here. spec.name, spec.providerRef,
	// and spec.import are already enforced immutable by validateSpec below, so
	// an Update can never introduce a new (server, name) collision that Create
	// didn't already see — running it here would only re-check a conflict that,
	// if present, was already present at Create. Blocking updates on it is
	// actively harmful: the reconciler's own routine updates (adding the
	// finalizer, stripping annotations, removing the finalizer on delete) go
	// through this same path, and rejecting them would make an already-flagged
	// duplicate impossible to finalize OR clean up — reintroducing the "manual
	// finalizer removal" symptom this fix exists to eliminate. The race-window
	// case (Create admitted before the conflict was resolvable) is instead
	// caught by the reconciler's findRealizedDuplicate backstop, which sets a
	// Ready=False/DuplicateOrganization condition via the status subresource
	// (not covered by this webhook) rather than blocking the object outright.
	return nil, v.validateSpec(org, old)
}

func (v *OrganizationValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *OrganizationValidator) validateSpec(org *uyuniv1.Organization, old *uyuniv1.Organization) error {
	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "organizations"}

	if org.Spec.Name == "" {
		return apierrors.NewForbidden(gr, org.Name, fmt.Errorf("spec.name is required"))
	}
	if org.Spec.ProviderRef.Name == "" {
		return apierrors.NewForbidden(gr, org.Name, fmt.Errorf("spec.providerRef.name is required"))
	}

	// Immutability guards (update only).
	if old != nil {
		if old.Spec.Name != "" && org.Spec.Name != old.Spec.Name {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.name is immutable after creation"))
		}
		if old.Spec.ProviderRef.Name != "" && org.Spec.ProviderRef.Name != old.Spec.ProviderRef.Name {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.providerRef.name is immutable after creation"))
		}
		oldImportID := 0
		if old.Spec.Import != nil {
			oldImportID = old.Spec.Import.OrganizationID
		}
		newImportID := 0
		if org.Spec.Import != nil {
			newImportID = org.Spec.Import.OrganizationID
		}
		if oldImportID != 0 && newImportID != oldImportID {
			return apierrors.NewForbidden(gr, org.Name,
				fmt.Errorf("spec.import.organizationId is immutable once set"))
		}
	}

	return nil
}

// checkDuplicate rejects an Organization whose (Uyuni server, name) already
// belongs to another non-import Organization CR. Without this, two
// BrandRegionClaims that happen to use the same organization name each get
// their own Organization CR, and the reconciler's adopt-by-name idempotency
// logic (meant to survive operator restarts, not to merge unrelated CRs)
// silently binds both to the same underlying Uyuni org — see
// organization_controller.go's findRealizedDuplicate for the reconcile-time
// backstop.
//
// Uniqueness is scoped by the UyuniProvider's resolved spec.url, not by
// spec.providerRef.name: each BrandRegionClaim gets its own privately named
// UyuniProvider object (see config/crossplane/composition.yaml), so two
// Organizations can share the same k8s providerRef name's *shape* while
// actually pointing at different servers, or have different providerRef
// names while pointing at the very same server. Only the latter is the
// actual conflict — comparing k8s object names would catch neither.
func (v *OrganizationValidator) checkDuplicate(ctx context.Context, org *uyuniv1.Organization) (admission.Warnings, error) {
	if org.Spec.Import != nil || org.Spec.Name == "" || org.Spec.ProviderRef.Name == "" {
		return nil, nil
	}

	myURL, err := v.providerURL(ctx, org.Spec.ProviderRef.Name)
	if err != nil {
		return admission.Warnings{
			"could not resolve spec.providerRef to verify org name uniqueness; will be re-validated at reconcile",
		}, nil
	}

	var list uyuniv1.OrganizationList
	if err := v.Client.List(ctx, &list); err != nil {
		return admission.Warnings{
			"could not list Organizations to verify uniqueness of spec.name; will be re-validated at reconcile",
		}, nil
	}

	gr := schema.GroupResource{Group: uyuniv1.Group, Resource: "organizations"}
	for _, other := range list.Items {
		if other.Namespace == org.Namespace && other.Name == org.Name {
			continue
		}
		if other.Spec.Import != nil || other.Spec.Name != org.Spec.Name {
			continue
		}
		otherURL, err := v.providerURL(ctx, other.Spec.ProviderRef.Name)
		if err != nil || otherURL != myURL {
			continue
		}
		return nil, apierrors.NewForbidden(gr, org.Name,
			fmt.Errorf("Organization %s/%s already uses name %q on Uyuni server %q; names must be unique per server (use spec.import to intentionally share an existing org)",
				other.Namespace, other.Name, org.Spec.Name, myURL))
	}
	return nil, nil
}

func (v *OrganizationValidator) providerURL(ctx context.Context, providerName string) (string, error) {
	var prov uyuniv1.UyuniProvider
	if err := v.Client.Get(ctx, client.ObjectKey{Name: providerName}, &prov); err != nil {
		return "", err
	}
	return prov.Spec.URL, nil
}
