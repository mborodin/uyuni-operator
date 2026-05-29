package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ConfigurationChannelSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	// Immutable after creation. Used as the Uyuni channel label.
	ID string `json:"id"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Enum=normal;state;dictionary
	// +kubebuilder:default=normal
	// Immutable after creation.
	Type string `json:"type"`

	// Name of the UyuniProvider to use. Empty → default provider.
	Cluster string `json:"cluster,omitempty"`

	// +kubebuilder:validation:Pattern=`^https?://.+`
	// Repository URL. Not forwarded to Uyuni; stored as operator metadata only.
	URL string `json:"url,omitempty"`
}

type ConfigurationChannelStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ID",type=string,JSONPath=`.spec.id`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Drift",type=string,JSONPath=`.status.conditions[?(@.type=='UyuniDrift')].status`
type ConfigurationChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConfigurationChannelSpec   `json:"spec,omitempty"`
	Status            ConfigurationChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ConfigurationChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigurationChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConfigurationChannel{}, &ConfigurationChannelList{})
}
