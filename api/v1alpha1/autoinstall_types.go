package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// --- AutoinstallDistribution ---

type AutoinstallDistributionSpec struct {
	// Label is the Cobbler tree label. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]*$`
	Label string `json:"label"`

	// BasePath is the absolute path to the distribution tree on the Uyuni server.
	// +kubebuilder:validation:Required
	BasePath string `json:"basePath"`

	// ChannelRef references the SoftwareChannel that provides packages for this distribution.
	// +kubebuilder:validation:Required
	ChannelRef LocalObjectRef `json:"channelRef"`

	// InstallType is the OS family identifier (e.g. "suse_leap15generic", "rhel_9").
	// Immutable after creation.
	// +kubebuilder:validation:Required
	InstallType string `json:"installType"`

	// KernelOptions are extra kernel command-line arguments appended at boot.
	KernelOptions string `json:"kernelOptions,omitempty"`

	// PostKernelOptions are kernel arguments appended after installation completes.
	PostKernelOptions string `json:"postKernelOptions,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type AutoinstallDistributionStatus struct {
	// UyuniID is the Cobbler tree ID assigned by Uyuni.
	UyuniID int `json:"uyuniId,omitempty"`
	// ChannelLabel is the realized software channel label used by this tree.
	ChannelLabel       string             `json:"channelLabel,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="UyuniID",type=integer,JSONPath=`.status.uyuniId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type AutoinstallDistribution struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AutoinstallDistributionSpec   `json:"spec,omitempty"`
	Status            AutoinstallDistributionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AutoinstallDistributionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoinstallDistribution `json:"items"`
}

// --- AutoinstallProfile ---

// AutoinstallScriptSpec describes a pre or post installation script added to the profile.
type AutoinstallScriptSpec struct {
	// Name uniquely identifies this script within the profile. Used as reconcile key.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Contents is the full script body.
	// +kubebuilder:validation:Required
	Contents string `json:"contents"`

	// Interpreter is the script interpreter path.
	// +kubebuilder:default="/bin/bash"
	Interpreter string `json:"interpreter,omitempty"`

	// Type controls when the script runs: pre (before install) or post (after install).
	// +kubebuilder:validation:Enum=pre;post
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// Chroot: if true, post scripts run chrooted to the installed system.
	Chroot bool `json:"chroot,omitempty"`

	// Template: if true, the script is rendered as a Cobbler template before execution.
	Template bool `json:"template,omitempty"`

	// ErrorOnFail: if true, installation aborts if the script exits non-zero.
	ErrorOnFail bool `json:"errorOnFail,omitempty"`
}

// ProfileScriptStatus records the Uyuni-assigned ID for a reconciled script.
type ProfileScriptStatus struct {
	// Name matches AutoinstallScriptSpec.Name and is used as the reconcile key.
	Name string `json:"name"`
	// UyuniID is the Uyuni-assigned script ID used to update or remove the script.
	UyuniID int `json:"uyuniId"`
}

type AutoinstallProfileSpec struct {
	// Label is the Cobbler profile label. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]*$`
	Label string `json:"label"`

	// DistributionRef references the AutoinstallDistribution providing the OS tree.
	// Immutable after creation.
	// +kubebuilder:validation:Required
	DistributionRef LocalObjectRef `json:"distributionRef"`

	// RootPasswordSecretRef references a Secret in the same namespace containing the
	// root account password set during installation.
	// +kubebuilder:validation:Required
	RootPasswordSecretRef SecretKeyRef `json:"rootPasswordSecretRef"`

	// VirtualizationType controls Cobbler's virtualization support for this profile.
	// +kubebuilder:validation:Enum=none;qemu;para_host;xenpv;xenfv
	// +kubebuilder:default=none
	VirtualizationType string `json:"virtualizationType,omitempty"`

	// KickstartHost is the Uyuni server hostname embedded in the kickstart file URL.
	// Defaults to the Uyuni server's configured hostname when empty.
	KickstartHost string `json:"kickstartHost,omitempty"`

	// ChildChannelRefs lists software channels to subscribe the system to during installation.
	ChildChannelRefs []LocalObjectRef `json:"childChannelRefs,omitempty"`

	// Variables are Cobbler template variables substituted into the kickstart file.
	// Ignored when KickstartContents is set.
	Variables map[string]string `json:"variables,omitempty"`

	// Scripts are pre/post installation scripts added to the profile.
	// Each script's Name field is the reconcile key.
	// Mutually exclusive with KickstartContents — ignored when KickstartContents is set.
	Scripts []AutoinstallScriptSpec `json:"scripts,omitempty"`

	// UpdateType controls whether packages are updated during installation.
	// +kubebuilder:validation:Enum=all;none
	// +kubebuilder:default=all
	UpdateType string `json:"updateType,omitempty"`

	// PreserveKsFile: if true, saves the kickstart file to /root/ks.cfg after install.
	PreserveKsFile bool `json:"preserveKsFile,omitempty"`

	// KickstartContents is the full kickstart/AutoYaST file provided inline.
	// When set, the reconciler calls kickstart.importFile instead of kickstart.createProfile.
	// Mutually exclusive with Scripts (webhook enforces).
	KickstartContents string `json:"kickstartContents,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type AutoinstallProfileStatus struct {
	Conditions         []metav1.Condition   `json:"conditions,omitempty"`
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	// ScriptIDs tracks Uyuni-assigned script IDs keyed by script Name.
	ScriptIDs          []ProfileScriptStatus `json:"scriptIds,omitempty"`
	// ChildChannelLabels is the realized list of child channel labels.
	ChildChannelLabels []string             `json:"childChannelLabels,omitempty"`
	// DistributionLabel is the resolved Cobbler tree label from spec.distributionRef.
	DistributionLabel  string               `json:"distributionLabel,omitempty"`
	// ContentsHash is the SHA-256 hex digest of spec.kickstartContents.
	// Used to detect changes and avoid re-importing an identical file.
	ContentsHash       string               `json:"contentsHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Distribution",type=string,JSONPath=`.status.distributionLabel`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type AutoinstallProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AutoinstallProfileSpec   `json:"spec,omitempty"`
	Status            AutoinstallProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AutoinstallProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoinstallProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&AutoinstallDistribution{}, &AutoinstallDistributionList{},
		&AutoinstallProfile{}, &AutoinstallProfileList{},
	)
}
