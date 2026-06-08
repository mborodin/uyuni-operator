package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
		&ConfigFile{}, &ConfigFileList{},
	)
}
