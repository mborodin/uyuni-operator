package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type GPGKey struct {
	URL         string `json:"url,omitempty"`
	KeyID       string `json:"keyId,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// +kubebuilder:default=true
	Check bool `json:"check"`
}

type SyncSchedule struct {
	// Quartz cron expression (7 fields including seconds and day-of-week
	// marker). Server timezone applies; no per-channel TZ in Uyuni.
	Cron string `json:"cron,omitempty"`

	// If true, controller schedules a one-off sync after channel creation.
	SyncOnCreate bool `json:"syncOnCreate,omitempty"`
}

type SoftwareChannelSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9._-]+$`
	// Immutable after creation. Webhook enforces.
	Label string `json:"label"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Summary string `json:"summary"`

	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Pattern=`^channel-(x86_64|aarch64|ppc64le|s390x|i386)$`
	// +kubebuilder:default=channel-x86_64
	// Immutable after creation.
	Arch string `json:"arch,omitempty"`

	// Immutable after creation (Uyuni doesn't allow reparenting).
	ParentChannelRef *LocalObjectRef `json:"parentChannelRef,omitempty"`

	// +kubebuilder:validation:Enum=sha1;sha256;sha384;sha512
	// +kubebuilder:default=sha256
	Checksum string `json:"checksum,omitempty"`

	GPGKey GPGKey `json:"gpgKey,omitempty"`

	RepositoryRefs []LocalObjectRef `json:"repositoryRefs,omitempty"`

	Sync SyncSchedule `json:"sync,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type SoftwareChannelStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	Label              string             `json:"label,omitempty"`
	AssociatedRepos    []string           `json:"associatedRepos,omitempty"`
	LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
	PackageCount       int                `json:"packageCount,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Packages",type=integer,JSONPath=`.status.packageCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Drift",type=string,JSONPath=`.status.conditions[?(@.type=='UyuniDrift')].status`
type SoftwareChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SoftwareChannelSpec   `json:"spec,omitempty"`
	Status            SoftwareChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SoftwareChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SoftwareChannel `json:"items"`
}

type RepositorySpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9._-]+$`
	// Immutable after creation.
	Label string `json:"label"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(https?|file|ftp|uln)://.+`
	URL string `json:"url"`

	// +kubebuilder:validation:Enum=yum;deb;uln
	// +kubebuilder:default=yum
	// Immutable after creation.
	Type string `json:"type,omitempty"`

	// Immutable after creation.
	HasSignedMetadata bool `json:"hasSignedMetadata,omitempty"`

	// Optional SSL client cert. Secret keys: ca.crt, tls.crt, tls.key
	// (standard kubernetes.io/tls + ca.crt convention).
	SSLCertRef *LocalObjectRef `json:"sslCertRef,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type RepositoryStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	Label              string             `json:"label,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.status.label`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Drift",type=string,JSONPath=`.status.conditions[?(@.type=='UyuniDrift')].status`
type Repository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RepositorySpec   `json:"spec,omitempty"`
	Status            RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&SoftwareChannel{}, &SoftwareChannelList{},
		&Repository{}, &RepositoryList{},
	)
}
