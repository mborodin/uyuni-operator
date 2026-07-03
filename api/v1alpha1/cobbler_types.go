package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CobblerMode selects how the operator manages a Cobbler object.
//
//	create: the operator creates and reconciles the object in Cobbler.
//	import: the operator only observes an object created elsewhere (e.g. by a
//	        Uyuni image build). It never creates or mutates it; it waits for the
//	        object to appear, then reflects it in status and becomes Ready.
//
// +kubebuilder:validation:Enum=create;import
type CobblerMode string

const (
	CobblerModeCreate CobblerMode = "create"
	CobblerModeImport CobblerMode = "import"
)

// --- CobblerDistro ---

type CobblerDistroSpec struct {
	// Name is the Cobbler distribution name. Immutable after creation.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:default=import
	Mode CobblerMode `json:"mode,omitempty"`

	// ProviderRef selects the UyuniProvider whose Cobbler this object lives in.
	// Empty uses the default provider.
	ProviderRef *LocalObjectRef `json:"providerRef,omitempty"`

	// Kernel is the absolute path to the kernel on the Cobbler server (create mode).
	Kernel string `json:"kernel,omitempty"`
	// Initrd is the absolute path to the initrd on the Cobbler server (create mode).
	Initrd string `json:"initrd,omitempty"`
	// Breed is the OS breed (e.g. "suse", "redhat"). Create mode.
	Breed string `json:"breed,omitempty"`
	// OSVersion is the OS version identifier. Create mode.
	OSVersion string `json:"osVersion,omitempty"`
	// Arch is the architecture (e.g. "x86_64"). Create mode.
	Arch string `json:"arch,omitempty"`
	// KernelOptions are extra kernel command-line options. Create mode.
	KernelOptions string `json:"kernelOptions,omitempty"`
	// AutoinstallMeta are Cobbler autoinstall metadata (ks_meta) variables. Create mode.
	AutoinstallMeta map[string]string `json:"autoinstallMeta,omitempty"`
}

type CobblerDistroStatus struct {
	// CobblerID is the Cobbler object's uid.
	CobblerID string `json:"cobblerId,omitempty"`
	// Found is true when the distribution exists in Cobbler.
	Found              bool               `json:"found,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=cobblerdistros,singular=cobblerdistro
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="CobblerName",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Found",type=boolean,JSONPath=`.status.found`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type CobblerDistro struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CobblerDistroSpec   `json:"spec,omitempty"`
	Status            CobblerDistroStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CobblerDistroList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CobblerDistro `json:"items"`
}

// --- CobblerProfile ---

type CobblerProfileSpec struct {
	// Name is the Cobbler profile name. Immutable after creation.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:default=import
	Mode CobblerMode `json:"mode,omitempty"`

	ProviderRef *LocalObjectRef `json:"providerRef,omitempty"`

	// DistroName is the Cobbler distribution this profile is based on (create mode).
	// Mutually exclusive with DistroRef.
	DistroName string `json:"distroName,omitempty"`
	// DistroRef references a CobblerDistro in the same namespace (create mode).
	// The operator resolves spec.name from it. Mutually exclusive with DistroName.
	DistroRef *LocalObjectRef `json:"distroRef,omitempty"`

	// Autoinstall is the autoinstall (kickstart/AutoYaST) template path on the
	// Cobbler server. Create mode.
	Autoinstall string `json:"autoinstall,omitempty"`
	// KernelOptions are extra kernel command-line options. Create mode.
	KernelOptions string `json:"kernelOptions,omitempty"`
	// AutoinstallMeta are Cobbler autoinstall metadata (ks_meta) variables. Create mode.
	AutoinstallMeta map[string]string `json:"autoinstallMeta,omitempty"`
	// EnableMenu controls whether the profile appears in the PXE menu. Create mode.
	EnableMenu *bool `json:"enableMenu,omitempty"`
}

type CobblerProfileStatus struct {
	CobblerID string `json:"cobblerId,omitempty"`
	Found     bool   `json:"found,omitempty"`
	// DistroName is the observed distribution the profile is based on.
	DistroName         string             `json:"distroName,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="CobblerName",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Distro",type=string,JSONPath=`.status.distroName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type CobblerProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CobblerProfileSpec   `json:"spec,omitempty"`
	Status            CobblerProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CobblerProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CobblerProfile `json:"items"`
}

// --- CobblerSystem ---

// CobblerInterface is one network interface of a Cobbler system record.
type CobblerInterface struct {
	// Name is the interface name (e.g. "eth0").
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
	MACAddress string `json:"macAddress,omitempty"`
	IPAddress  string `json:"ipAddress,omitempty"`
	DNSName    string `json:"dnsName,omitempty"`
}

type CobblerSystemSpec struct {
	// Name is the Cobbler system record name (Uyuni uses "<fqdn>:<orgId>").
	// Immutable after creation.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:default=import
	Mode CobblerMode `json:"mode,omitempty"`

	ProviderRef *LocalObjectRef `json:"providerRef,omitempty"`

	// ProfileName is the Cobbler profile the system boots (create mode).
	// Mutually exclusive with ProfileRef.
	ProfileName string `json:"profileName,omitempty"`
	// ProfileRef references a CobblerProfile in the same namespace (create mode).
	ProfileRef *LocalObjectRef `json:"profileRef,omitempty"`

	// Interfaces are the system's network interfaces (create mode). Named
	// interfaces are set via Cobbler's modify_interface (macaddress-<name>, ...).
	Interfaces []CobblerInterface `json:"interfaces,omitempty"`

	// AutoinstallMeta are the Cobbler autoinstall metadata (ks_meta) variables
	// set on the system record. Create mode. system.setVariables-equivalent.
	AutoinstallMeta map[string]string `json:"autoinstallMeta,omitempty"`

	// NetbootEnabled toggles PXE netboot on the record. Defaults to true. Create mode.
	// +kubebuilder:default=true
	NetbootEnabled *bool `json:"netbootEnabled,omitempty"`

	// Server is the Cobbler "server override" (e.g. a proxy the system boots
	// through). Create mode.
	Server string `json:"server,omitempty"`

	// Comment is a free-text comment on the record. Create mode.
	Comment string `json:"comment,omitempty"`
}

type CobblerSystemStatus struct {
	CobblerID string `json:"cobblerId,omitempty"`
	Found     bool   `json:"found,omitempty"`
	// ProfileName is the observed profile the system is bound to.
	ProfileName        string             `json:"profileName,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="CobblerName",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.status.profileName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type CobblerSystem struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CobblerSystemSpec   `json:"spec,omitempty"`
	Status            CobblerSystemStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CobblerSystemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CobblerSystem `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&CobblerDistro{}, &CobblerDistroList{},
		&CobblerProfile{}, &CobblerProfileList{},
		&CobblerSystem{}, &CobblerSystemList{},
	)
}
