package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// =============================================================================
// Proxy
// =============================================================================

// ProxySpec is the declarative configuration for a Uyuni containerized proxy.
// The operator calls proxy.containerConfig to generate the proxy's config
// archive (tar.gz) and stores the extracted files in an owned Secret named in
// status. Spec is intent; status records what was generated.
type ProxySpec struct {
	// ProviderRef selects the UyuniProvider whose server generates the proxy
	// configuration.
	// +kubebuilder:validation:Required
	ProviderRef LocalObjectRef `json:"providerRef"`

	// FQDN is the proxy's own fully-qualified domain name. Immutable — it is the
	// proxy's identity; changing it would produce an unrelated configuration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	FQDN string `json:"fqdn"`

	// ParentServer is the FQDN of the server the proxy connects to. When empty
	// the operator uses the UyuniProvider's server host.
	// +optional
	ParentServer string `json:"parentServer,omitempty"`

	// SSHPort is the SSH port the proxy listens on for the server push tunnel.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8022
	// +optional
	SSHPort int `json:"sshPort,omitempty"`

	// MaxCacheMB is the maximum squid cache size in megabytes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=102400
	// +optional
	MaxCacheMB int `json:"maxCacheMB,omitempty"`

	// Email is the proxy administrator's email, embedded in the generated config.
	// +optional
	Email string `json:"email,omitempty"`

	// TLSSecretRef references a kubernetes.io/tls Secret in the same namespace
	// holding the proxy certificate. Expected keys: tls.crt, tls.key, ca.crt,
	// and an optional ca-intermediate.crt (PEM bundle) for intermediate CAs.
	// When unset, the server reuses its own certificate (no-cert generation).
	// +optional
	TLSSecretRef *LocalObjectRef `json:"tlsSecretRef,omitempty"`
}

// ProxyStatus records the generated proxy configuration. The archive's files
// (including private keys) live in the owned Secret, never in status; status
// inlines only the non-sensitive config for observability.
type ProxyStatus struct {
	// SecretName is the operator-owned Secret holding the extracted archive
	// files (one key per archive member). Deleted with the Proxy via GC.
	SecretName string `json:"secretName,omitempty"`

	// Files lists the archive member names (sorted) present in the Secret.
	Files []string `json:"files,omitempty"`

	// Config inlines the contents of the non-sensitive archive members only
	// (e.g. config.yaml). Never contains certificate keys or SSH private keys.
	Config string `json:"config,omitempty"`

	// InputHash is a hash of the resolved generation inputs. Regeneration (which
	// rotates the proxy SSH keypair) happens only when this changes or the
	// regenerate annotation is set — not on every reconcile.
	InputHash string `json:"inputHash,omitempty"`

	// GeneratedAt is when the current archive was generated.
	GeneratedAt *metav1.Time `json:"generatedAt,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="FQDN",type=string,JSONPath=`.spec.fqdn`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.parentServer`
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.secretName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type Proxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProxySpec   `json:"spec,omitempty"`
	Status            ProxyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Proxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Proxy{}, &ProxyList{})
}
