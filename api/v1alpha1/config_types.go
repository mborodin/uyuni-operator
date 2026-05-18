package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ConfigChannelSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	// Immutable after creation.
	Label string `json:"label"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Enum=normal;state;dictionary
	// +kubebuilder:default=normal
	// Immutable after creation.
	Type string `json:"type"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ConfigChannelStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	FileCount          int                `json:"fileCount,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Files",type=integer,JSONPath=`.status.fileCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Drift",type=string,JSONPath=`.status.conditions[?(@.type=='UyuniDrift')].status`
type ConfigChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConfigChannelSpec   `json:"spec,omitempty"`
	Status            ConfigChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ConfigChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigChannel `json:"items"`
}

type ConfigFileSpec struct {
	// +kubebuilder:validation:Required
	ChannelRef LocalObjectRef `json:"channelRef"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`

	// +kubebuilder:validation:Enum=file;directory;symlink
	// +kubebuilder:default=file
	Type string `json:"type"`

	// One of: Contents (inline), ContentsFrom (Secret), TargetPath (symlink only).
	Contents     string          `json:"contents,omitempty"`
	ContentsFrom *LocalObjectRef `json:"contentsFrom,omitempty"`
	TargetPath   string          `json:"targetPath,omitempty"`

	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	// +kubebuilder:validation:Pattern=`^[0-7]{3,4}$`
	Permissions    string `json:"permissions,omitempty"`
	SELinuxContext string `json:"selinuxContext,omitempty"`

	// Render as Salt template at deploy time (state channels only).
	Macro bool `json:"templateMacro,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ConfigFileStatus struct {
	Revision           int                `json:"revision,omitempty"`
	ContentHash        string             `json:"contentHash,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=`.spec.channelRef.name`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Rev",type=integer,JSONPath=`.status.revision`
type ConfigFile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConfigFileSpec   `json:"spec,omitempty"`
	Status            ConfigFileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ConfigFileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigFile `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&ConfigChannel{}, &ConfigChannelList{},
		&ConfigFile{}, &ConfigFileList{},
	)
}
