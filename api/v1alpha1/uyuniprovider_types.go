package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type UyuniProviderSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	URL string `json:"url"`

	// Reference to a Secret containing keys `username` and `password`.
	// The secret MUST live in the operator's own namespace (default:
	// uyuni-operator-system). Cross-namespace refs are rejected.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.SecretReference `json:"credentialsSecretRef"`

	// Skip TLS verification. Homelab use only.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Optional CA bundle for self-signed servers. Secret key: ca.crt.
	CACertSecretRef *corev1.SecretReference `json:"caCertSecretRef,omitempty"`

	// +kubebuilder:default="30s"
	Timeout metav1.Duration `json:"timeout,omitempty"`

	// At most one provider per cluster may set this true. Webhook enforces;
	// reconciler also checks as a race-window backstop.
	IsDefault bool `json:"isDefault,omitempty"`
}

type UyuniProviderStatus struct {
	ServerVersion      string             `json:"serverVersion,omitempty"`
	OrgID              int                `json:"orgId,omitempty"`
	LastReachableTime  *metav1.Time       `json:"lastReachableTime,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.serverVersion`
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.isDefault`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type UyuniProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              UyuniProviderSpec   `json:"spec,omitempty"`
	Status            UyuniProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type UyuniProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UyuniProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UyuniProvider{}, &UyuniProviderList{})
}
