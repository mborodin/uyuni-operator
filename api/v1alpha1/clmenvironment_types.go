package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ClmEnvironmentSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	// Immutable after creation. Environment label in Uyuni (the "id")
	Id string `json:"id"`

	// +kubebuilder:validation:Required
	// Human-readable environment name (mutable)
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Required
	// Reference to parent ContentProject (immutable)
	ProjectRef LocalObjectRef `json:"projectRef"`

	// +kubebuilder:validation:Optional
	// Label of predecessor environment in promotion chain (empty = root)
	Predecessor string `json:"predecessor,omitempty"`

	// +kubebuilder:validation:Optional
	// Reference to UyuniProvider (cluster). Nil = use default provider
	Cluster *LocalObjectRef `json:"cluster,omitempty"`

	// +kubebuilder:validation:Optional
	// Reference to Organization (may be inherited from project)
	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ClmEnvironmentStatus struct {
	// Environment ID assigned by Uyuni (matches spec.id when reconciled)
	UyuniLabel string `json:"uyuniLabel,omitempty"`

	// Current environment state in Uyuni
	State string `json:"state,omitempty"` // NEW, BUILDING, BUILT, FAILED, etc.

	// Built version number
	BuiltVersion int `json:"builtVersion,omitempty"`

	// Reconciliation conditions
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Id",type=string,JSONPath=`.spec.id`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type ClmEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClmEnvironmentSpec   `json:"spec,omitempty"`
	Status            ClmEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ClmEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClmEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&ClmEnvironment{}, &ClmEnvironmentList{},
	)
}
