package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AutoinstallSpec configures Cobbler/Kickstart-based OS provisioning via Uyuni.
type AutoinstallSpec struct {
	// Profile is the Cobbler/Kickstart profile name registered in Uyuni.
	// Immutable once the first provisioning action has been scheduled.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Profile string `json:"profile"`

	// Earliest is the earliest time at which to schedule the autoinstall action.
	// Defaults to immediate execution.
	Earliest *metav1.Time `json:"earliest,omitempty"`
}

type NetworkInterface struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
	MACAddress string `json:"macAddress"`

	IPAddress string `json:"ipAddress,omitempty"`
}

type SystemSpec struct {
	// +kubebuilder:validation:Required
	// Immutable after creation.
	MinionID string `json:"minionId"`

	// If true, create an empty system profile in Uyuni before the system
	// boots, so configuration applies on first registration. Requires at
	// least one of: network[].macAddress, hostname.
	PreCreate bool `json:"preCreate,omitempty"`

	// Hostname used in the pre-created profile. Defaults to minionId via
	// the SystemDefaulter mutating webhook.
	Hostname string `json:"hostname,omitempty"`

	Network []NetworkInterface `json:"network,omitempty"`

	Description string `json:"description,omitempty"`

	BaseChannelRef    *LocalObjectRef      `json:"baseChannelRef,omitempty"`
	BaseChannelFrom   *ChannelFromProject  `json:"baseChannelFrom,omitempty"`
	ChildChannelRefs  []LocalObjectRef     `json:"childChannelRefs,omitempty"`
	ChildChannelsFrom []ChannelFromProject `json:"childChannelsFrom,omitempty"`

	CustomInfo map[string]string `json:"customInfo,omitempty"`

	// AddOns are additional entitlements granted to the system, e.g.
	// "virtualization_host", "container_build_host", "osimage_build_host",
	// "ansible_control_node", "monitoring_entitled".
	AddOns []string `json:"addOns,omitempty"`

	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// Defaulted by webhook: 24h if PreCreate, 30m otherwise.
	AdoptionTimeout metav1.Duration `json:"adoptionTimeout,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`

	// ConfigChannelRefs lists config channels subscribed directly to this system,
	// in priority order (index 0 = highest). These always outrank channels
	// inherited from spec.groupRefs.
	ConfigChannelRefs []LocalObjectRef `json:"configChannelRefs,omitempty"`

	// GroupRefs declares which SystemGroups this system should belong to.
	// The reconciler adds/removes group membership to match this list.
	// Preferred over SystemGroup.spec.memberRefs (system-side declaration).
	GroupRefs []LocalObjectRef `json:"groupRefs,omitempty"`

	// Autoinstall configures Cobbler/Kickstart-based OS provisioning. When set,
	// the reconciler calls system.provisionSystem after the system profile is
	// pre-created (preCreate must also be true for initial provisioning).
	Autoinstall *AutoinstallSpec `json:"autoinstall,omitempty"`

	// ContactMethod controls how the Uyuni server communicates with this system.
	// +kubebuilder:validation:Enum=default;ssh-push;ssh-push-tunnel
	// +kubebuilder:default=default
	ContactMethod string `json:"contactMethod,omitempty"`
}

type SystemStatus struct {
	UyuniServerID int `json:"uyuniServerId,omitempty"`

	// +kubebuilder:validation:Enum=Pending;PreProvisioned;Reprovisioning;Registered;Reconciled
	Phase string `json:"phase,omitempty"`

	// PhaseTransitionTime records when Phase last changed. Used to enforce AdoptionTimeout.
	PhaseTransitionTime *metav1.Time `json:"phaseTransitionTime,omitempty"`

	BaseChannelLabel   string       `json:"baseChannelLabel,omitempty"`
	ChildChannelLabels []string     `json:"childChannelLabels,omitempty"`
	ActiveAddOns       []string     `json:"activeAddOns,omitempty"`
	LastCheckinTime    *metav1.Time `json:"lastCheckinTime,omitempty"`

	// ConfigChannelLabels is the realized ordered config channel subscription
	// (direct refs first, then group-sourced). Used to detect and apply drift.
	ConfigChannelLabels []string `json:"configChannelLabels,omitempty"`

	// GroupNames is the set of Uyuni group names the system was last placed in.
	// Used to compute removals on next reconcile.
	GroupNames []string `json:"groupNames,omitempty"`

	// AutoinstallActionID is the Uyuni action ID of the last scheduled provisioning.
	AutoinstallActionID int `json:"autoinstallActionId,omitempty"`

	// AutoinstallStatus reflects the Uyuni-side outcome of the last provisioning action.
	// +kubebuilder:validation:Enum=Scheduled;Completed;Failed
	AutoinstallStatus string `json:"autoinstallStatus,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MinionID",type=string,JSONPath=`.spec.minionId`
// +kubebuilder:printcolumn:name="UyuniID",type=integer,JSONPath=`.status.uyuniServerId`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="BaseChannel",type=string,JSONPath=`.status.baseChannelLabel`
// +kubebuilder:printcolumn:name="LastCheckin",type=date,JSONPath=`.status.lastCheckinTime`
// +kubebuilder:printcolumn:name="BuildHost",type=string,JSONPath=`.status.conditions[?(@.type=='BuildHost')].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type System struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SystemSpec   `json:"spec,omitempty"`
	Status            SystemStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SystemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []System `json:"items"`
}

type SystemGroupSpec struct {
	// +kubebuilder:validation:Required
	// Immutable after creation (no rename in Uyuni).
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// Deprecated: use System.spec.groupRefs instead. System-side declaration is
	// the authoritative source; this field is retained for backward compatibility.
	MemberRefs        []LocalObjectRef `json:"memberRefs,omitempty"`
	StaticMinionIDs   []string         `json:"staticMinionIds,omitempty"`
	ConfigChannelRefs []LocalObjectRef `json:"configChannelRefs,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`

	// +kubebuilder:validation:Optional
	// Reference to UyuniProvider for multi-cluster support.
	// Empty = use default provider (spec.isDefault: true)
	Cluster *LocalObjectRef `json:"cluster,omitempty"`
}

type SystemGroupStatus struct {
	UyuniID                   int                `json:"uyuniId,omitempty"`
	MemberCount               int                `json:"memberCount,omitempty"`
	ResolvedMembers           []string           `json:"resolvedMembers,omitempty"`
	ActiveConfigChannelLabels []string           `json:"activeConfigChannelLabels,omitempty"`
	ObservedGeneration        int64              `json:"observedGeneration,omitempty"`
	Conditions                []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.memberCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type SystemGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SystemGroupSpec   `json:"spec,omitempty"`
	Status            SystemGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SystemGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SystemGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&System{}, &SystemList{},
		&SystemGroup{}, &SystemGroupList{},
	)
}
