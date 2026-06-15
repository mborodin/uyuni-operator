package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BrandRegionProvider defines the UyuniProvider created for this region.
// The reconciler creates a cluster-scoped UyuniProvider named after the BrandRegion.
type BrandRegionProvider struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	URL string `json:"url"`

	// CredentialsSecretRef references a Secret in the operator namespace with
	// keys `username` and `password` for the Uyuni automation account.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.SecretReference `json:"credentialsSecretRef"`

	// InsecureSkipVerify disables TLS verification. Homelab / self-signed use only.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// CACertSecretRef references a Secret containing a `ca.crt` key for
	// self-signed Uyuni servers.
	CACertSecretRef *corev1.SecretReference `json:"caCertSecretRef,omitempty"`

	// +kubebuilder:default="30s"
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// BrandRegionOrganization defines the Uyuni organization owned by a BrandRegion.
// The reconciler creates an Organization CR named after the BrandRegion.
type BrandRegionOrganization struct {
	// Name of the organization in Uyuni (e.g. "Delhaize").
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// CredentialsSecretRef references a Secret with org admin credentials
	// (keys: username, password). Required when creating a new org.
	CredentialsSecretRef *LocalObjectRef `json:"credentialsSecretRef,omitempty"`

	// Import links this to a pre-existing Uyuni organization instead of
	// creating a new one. When set, CredentialsSecretRef is optional.
	Import *OrganizationImport `json:"import,omitempty"`
}

// BrandRegionRepository wraps a Repository CR to be owned by the BrandRegion.
type BrandRegionRepository struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Spec is passed through to the Repository CR. OrganizationRef is auto-set
	// to the BrandRegion's managed organization.
	Spec RepositorySpec `json:"spec"`
}

// BrandRegionSoftwareChannel wraps a SoftwareChannel CR owned by the BrandRegion.
type BrandRegionSoftwareChannel struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Spec is passed through to the SoftwareChannel CR. OrganizationRef is
	// auto-set to the BrandRegion's managed organization.
	Spec SoftwareChannelSpec `json:"spec"`
}

// BrandRegionConfigChannel wraps a ConfigurationChannel CR owned by the BrandRegion.
type BrandRegionConfigChannel struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Spec is passed through to the ConfigurationChannel CR.
	Spec ConfigurationChannelSpec `json:"spec"`
}

// BrandRegionGroup defines a Uyuni system group owned by a BrandRegion.
// The reconciler creates a SystemGroup CR for each entry.
type BrandRegionGroup struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// SystemType classifies the intended role of members (informational).
	// +kubebuilder:validation:Enum=pos;storehub;branchserver
	SystemType SystemType `json:"systemType,omitempty"`

	// ConfigChannelRefs assigns configuration channels to the group (priority order).
	ConfigChannelRefs []LocalObjectRef `json:"configChannelRefs,omitempty"`
}

// BrandRegionActivationKey defines an activation key owned by a BrandRegion.
// The reconciler creates an ActivationKey CR for each entry.
type BrandRegionActivationKey struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// SystemGroupRefs links this key to system groups.
	SystemGroupRefs []LocalObjectRef `json:"systemGroupRefs,omitempty"`

	// Entitlements lists add-on entitlements granted via this key.
	Entitlements []string `json:"entitlements,omitempty"`
}

// BrandRegionStateRepository references the Git repository from which Salt
// minions pull their state. This is stored as a BrandRegion-level declaration;
// the operator makes it available to ConfigurationChannels of type "state"
// and exposes it in status for downstream consumers.
type BrandRegionStateRepository struct {
	// URL of the Git repository. Supports HTTPS (https://) and SSH (ssh://, git@...) formats.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Branch to check out. Defaults to "main".
	// +kubebuilder:default="main"
	Branch string `json:"branch,omitempty"`

	// SubPath is the directory within the repository that contains Salt states.
	// Defaults to the repository root.
	SubPath string `json:"subPath,omitempty"`

	// CredentialsSecretRef references a Secret with Git credentials.
	// For HTTPS: keys "username" and "password".
	// For SSH: key "sshPrivateKey".
	CredentialsSecretRef *corev1.SecretReference `json:"credentialsSecretRef,omitempty"`
}

// BrandRegionSystemCount holds per-type system counters tracked in status.
type BrandRegionSystemCount struct {
	BranchServer int `json:"branchserver,omitempty"`
	StoreHub     int `json:"storehub,omitempty"`
	POS          int `json:"pos,omitempty"`
	Total        int `json:"total,omitempty"`
}

type BrandRegionSpec struct {
	// Brand identifier (e.g. "delhaize"). Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Brand string `json:"brand"`

	// Region identifier (e.g. "emea"). Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Region string `json:"region"`

	// Provider defines the UyuniProvider created for this region.
	// A cluster-scoped UyuniProvider CR named after the BrandRegion is created.
	// +kubebuilder:validation:Required
	Provider BrandRegionProvider `json:"provider"`

	// Organization defines the Uyuni organization owned by this BrandRegion.
	// An Organization CR named after the BrandRegion is created.
	// +kubebuilder:validation:Required
	Organization BrandRegionOrganization `json:"organization"`

	// Repositories lists the Repository CRs to create as children.
	Repositories []BrandRegionRepository `json:"repositories,omitempty"`

	// SoftwareChannels lists the SoftwareChannel CRs to create as children.
	SoftwareChannels []BrandRegionSoftwareChannel `json:"softwareChannels,omitempty"`

	// ConfigChannels lists the ConfigurationChannel CRs to create as children.
	ConfigChannels []BrandRegionConfigChannel `json:"configChannels,omitempty"`

	// BaseChannelRef is the base software channel for all managed ActivationKeys.
	// Typically references one of the SoftwareChannels defined above.
	// Mutually exclusive with BaseChannelFrom.
	BaseChannelRef *LocalObjectRef `json:"baseChannelRef,omitempty"`

	// BaseChannelFrom resolves the base channel via a ContentProject environment.
	// Mutually exclusive with BaseChannelRef.
	BaseChannelFrom *ChannelFromProject `json:"baseChannelFrom,omitempty"`

	ChildChannelRefs  []LocalObjectRef     `json:"childChannelRefs,omitempty"`
	ChildChannelsFrom []ChannelFromProject `json:"childChannelsFrom,omitempty"`

	// SystemGroups lists the Uyuni system groups owned by this BrandRegion.
	// A SystemGroup CR is created for each entry.
	SystemGroups []BrandRegionGroup `json:"systemGroups,omitempty"`

	// ActivationKeys lists the activation keys owned by this BrandRegion.
	// An ActivationKey CR is created for each entry, pre-wired with base channel
	// and organization from this BrandRegion.
	ActivationKeys []BrandRegionActivationKey `json:"activationKeys,omitempty"`

	// StateRepository is the Git repository from which Salt minions in this
	// region pull their configuration state. Stored as metadata on the BrandRegion;
	// use it to wire ConfigurationChannels of type "state" to a centralised repo.
	StateRepository *BrandRegionStateRepository `json:"stateRepository,omitempty"`
}

type BrandRegionStatus struct {
	// SystemCount tracks registered systems per type in this namespace.
	SystemCount BrandRegionSystemCount `json:"systemCount,omitempty"`

	// ManagedProvider is the name of the UyuniProvider CR created by this BrandRegion.
	ManagedProvider string `json:"managedProvider,omitempty"`

	// ManagedOrganization is the name of the Organization CR created by this BrandRegion.
	ManagedOrganization string `json:"managedOrganization,omitempty"`

	// ManagedRepositories lists the Repository CR names created by this BrandRegion.
	ManagedRepositories []string `json:"managedRepositories,omitempty"`

	// ManagedSoftwareChannels lists the SoftwareChannel CR names created by this BrandRegion.
	ManagedSoftwareChannels []string `json:"managedSoftwareChannels,omitempty"`

	// ManagedConfigChannels lists the ConfigurationChannel CR names created by this BrandRegion.
	ManagedConfigChannels []string `json:"managedConfigChannels,omitempty"`

	// ManagedGroups lists the SystemGroup CR names created by this BrandRegion.
	ManagedGroups []string `json:"managedGroups,omitempty"`

	// ManagedActivationKeys lists the ActivationKey CR names created by this BrandRegion.
	ManagedActivationKeys []string `json:"managedActivationKeys,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Brand",type=string,JSONPath=`.spec.brand`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="BranchServers",type=integer,JSONPath=`.status.systemCount.branchserver`
// +kubebuilder:printcolumn:name="StoreHubs",type=integer,JSONPath=`.status.systemCount.storehub`
// +kubebuilder:printcolumn:name="POS",type=integer,JSONPath=`.status.systemCount.pos`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type BrandRegion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BrandRegionSpec   `json:"spec,omitempty"`
	Status            BrandRegionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BrandRegionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BrandRegion `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BrandRegion{}, &BrandRegionList{})
}
