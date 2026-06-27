package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CustomInfoKeySpec defines an organization-level custom system info key in
// Uyuni (system.custominfo). Systems reference these keys via
// System.spec.customInfoValues and supply per-system values.
type CustomInfoKeySpec struct {
	// Label is the custom info key label in Uyuni. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	Label string `json:"label"`

	// Description is a human-readable description of the key.
	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type CustomInfoKeyStatus struct {
	// UyuniID is the custom info key ID assigned by Uyuni.
	UyuniID            int                `json:"uyuniId,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="UyuniID",type=integer,JSONPath=`.status.uyuniId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type CustomInfoKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CustomInfoKeySpec   `json:"spec,omitempty"`
	Status            CustomInfoKeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CustomInfoKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CustomInfoKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CustomInfoKey{}, &CustomInfoKeyList{})
}
