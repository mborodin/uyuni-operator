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

	// Name of the Organization CR whose credentials to use when creating this channel.
	// When set, the channel is created in that organization's context (org-scoped).
	OrganizationRef string `json:"organizationRef,omitempty"`

	// +kubebuilder:validation:Pattern=`^https?://.+`
	// Repository URL for automatic file sync. Used with repositoryType for syncing files from external repositories.
	URL string `json:"url,omitempty"`

	// +kubebuilder:validation:Enum=git;http;local
	// +kubebuilder:default=git
	// Type of repository for syncing files. Only used if autoSync is enabled.
	RepositoryType string `json:"repositoryType,omitempty"`

	// +kubebuilder:default=true
	// Whether to automatically sync files from the repository URL into this channel.
	AutoSync *bool `json:"autoSync,omitempty"`

	// Branch, tag, or ref to sync from (for git repositories).
	// Examples: "main", "develop", "v1.0.0"
	RepositoryRef string `json:"repositoryRef,omitempty"`

	// Sub-path within repository to sync from (if not root).
	// Examples: "salt/baseline", "configs", "files"
	RepositoryPath string `json:"repositoryPath,omitempty"`

	// Auth injects username/password Basic Auth into the repository URL (URL)
	// at reconcile time. Credentials are read from the named Secret and never stored in status.
	// +optional
	Auth *BasicAuthRef `json:"auth,omitempty"`

	// +kubebuilder:validation:Pattern=`^(@(hourly|daily|weekly)|every \d+[mh]|0 .* .* .* .*)?$`
	// Cron schedule for repository syncs. If empty, syncs only on reconciliation.
	// Examples: "0 */6 * * *" (every 6 hours), "@daily" (daily), "every 2h" (every 2 hours)
	SyncSchedule string `json:"syncSchedule,omitempty"`
}

type ConfigurationChannelStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	// Last time repository was synced
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Number of configuration files currently synced from repository
	SyncedFileCount int `json:"syncedFileCount,omitempty"`

	// +kubebuilder:validation:Enum=Synced;Syncing;Failed;NotConfigured
	// Current synchronization status of repository files
	SyncStatus string `json:"syncStatus,omitempty"`

	// Hash of repository content (to detect changes between syncs)
	RepositoryHash string `json:"repositoryHash,omitempty"`

	// List of file paths synced in the last successful sync
	SyncedFiles []string `json:"syncedFiles,omitempty"`
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
