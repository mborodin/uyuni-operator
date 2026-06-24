package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OrganizationImport links this CR to a pre-existing Uyuni organization
// instead of creating a new one. When set, the operator adopts the org
// and will not delete it when the CR is deleted.
type OrganizationImport struct {
	// +kubebuilder:validation:Minimum=1
	OrganizationID int `json:"organizationId"`
}

type OrganizationSpec struct {
	// Name of the organization in Uyuni. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reference to the UyuniProvider used for org-level operations
	// (satellite admin credentials). Required.
	// +kubebuilder:validation:Required
	ProviderRef LocalObjectRef `json:"providerRef"`

	// Optional credentials for the org admin user. The Secret must exist
	// in the same namespace as the Organization and contain 'username' and
	// 'password' keys. When creating a new org (spec.import omitted),
	// optional 'firstName', 'lastName', and 'email' keys name the initial
	// admin user (defaults: "Org", "Admin", "<username>@uyuni.local").
	// When set, resources that reference this Organization connect to
	// Uyuni as this user; otherwise the provider's satellite admin is used.
	// Required when spec.import is omitted (new org creation).
	CredentialsSecretRef *LocalObjectRef `json:"credentialsSecretRef,omitempty"`

	// When set, links this CR to an existing Uyuni organization instead
	// of creating one. The org with the given ID must already exist.
	Import *OrganizationImport `json:"import,omitempty"`
}

type OrganizationStatus struct {
	UyuniOrgID         int                `json:"uyuniOrgId,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	// ProviderURL, ProviderInsecureSkipVerify, ProviderCredentialsSecretRef, and
	// ProviderCACertSecretRef snapshot the UyuniProvider's connection details the
	// last time it was successfully resolved. Deletion uses this snapshot instead
	// of re-reading the UyuniProvider CR, since Crossplane compositions may delete
	// sibling managed resources (including the UyuniProvider) concurrently with
	// the Organization, before the Organization's own Uyuni-side cleanup runs.
	ProviderURL                  string                  `json:"providerUrl,omitempty"`
	ProviderInsecureSkipVerify   bool                    `json:"providerInsecureSkipVerify,omitempty"`
	ProviderCredentialsSecretRef *corev1.SecretReference `json:"providerCredentialsSecretRef,omitempty"`
	ProviderCACertSecretRef      *corev1.SecretReference `json:"providerCaCertSecretRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="OrgName",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="OrgID",type=integer,JSONPath=`.status.uyuniOrgId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Organization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OrganizationSpec   `json:"spec,omitempty"`
	Status            OrganizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Organization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Organization{}, &OrganizationList{})
}
