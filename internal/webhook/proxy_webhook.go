package webhook

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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
)

// +kubebuilder:webhook:path=/validate-uyuni-uyuni-project-org-v1alpha1-proxy,mutating=false,failurePolicy=fail,sideEffects=None,groups=uyuni.uyuni-project.org,resources=proxies,verbs=create;update,versions=v1alpha1,name=vproxy.uyuni.uyuni-project.org,admissionReviewVersions=v1

type ProxyValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &ProxyValidator{}

func (v *ProxyValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&uyuniv1.Proxy{}).
		WithValidator(v).
		Complete()
}

func (v *ProxyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj.(*uyuniv1.Proxy)), nil
}

func (v *ProxyValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldProxy := oldObj.(*uyuniv1.Proxy)
	newProxy := newObj.(*uyuniv1.Proxy)
	// The FQDN is the proxy's identity; changing it would generate an unrelated
	// configuration rather than reconfigure this proxy.
	if oldProxy.Spec.FQDN != newProxy.Spec.FQDN {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: uyuniv1.Group, Kind: "Proxy"},
			newProxy.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "fqdn"), "fqdn is immutable")})
	}
	return v.validate(ctx, newProxy), nil
}

func (v *ProxyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate returns advisory warnings only (never blocks) for cross-resource
// concerns: a referenced TLS Secret that doesn't exist yet is a warning, since
// GitOps may apply it after the Proxy. Structural checks are handled by the
// OpenAPI markers on the type.
func (v *ProxyValidator) validate(ctx context.Context, proxy *uyuniv1.Proxy) admission.Warnings {
	if proxy.Spec.TLSSecretRef == nil {
		return nil
	}
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Spec.TLSSecretRef.Name}
	if err := v.Client.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Warnings{fmt.Sprintf(
				"TLS secret %q not found in namespace %q; proxy config generation will wait for it",
				proxy.Spec.TLSSecretRef.Name, proxy.Namespace)}
		}
		// API-server problems must not block admission.
		return nil
	}
	return nil
}
