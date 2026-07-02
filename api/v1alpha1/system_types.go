package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:Enum=pos;storehub;branchserver
type SystemType string

const (
	SystemTypePOS          SystemType = "pos"
	SystemTypeStoreHub     SystemType = "storehub"
	SystemTypeBranchServer SystemType = "branchserver"
)

// AutoinstallSpec configures Cobbler/Kickstart-based OS provisioning via Uyuni.
// Exactly one of Profile or ProfileRef must be set; the webhook enforces mutual exclusion.
type AutoinstallSpec struct {
	// Profile is the bare Cobbler/Kickstart profile label registered in Uyuni.
	// Immutable once the first provisioning action has been scheduled.
	// Mutually exclusive with ProfileRef.
	Profile string `json:"profile,omitempty"`

	// ProfileRef references an AutoinstallProfile CR in the same namespace.
	// The reconciler resolves spec.label from the CR at runtime.
	// Mutually exclusive with Profile.
	ProfileRef *LocalObjectRef `json:"profileRef,omitempty"`

	// Netboot enables PXE netboot on the Cobbler system record — the netboot
	// flag of system.setVariables, matching the UI's "Enable netboot" toggle.
	// Defaults to true. Applied together with Variables (system.setVariables
	// couples the two), so it only takes effect when at least one variable is
	// declared; otherwise createSystemRecord's default (netboot on) stands.
	// +kubebuilder:default=true
	Netboot *bool `json:"netboot,omitempty"`

	// Variables are per-system Cobbler system-record variables (ks_meta),
	// substituted into the autoinstall template. Each entry sets a literal
	// Value or sources it from a Secret/ConfigMap key (like a pod env var).
	// Applied via system.setVariables when PreCreate is true and at least one
	// variable is declared. IMPORTANT: system.setVariables REPLACES the record's
	// entire ks_meta set, so this must be the COMPLETE authoritative set — any
	// Uyuni-generated key you want to keep (e.g. bootstrap_token, activation_key)
	// must be included here, typically sourced from a Secret. Explicit Variables
	// override keys imported by VariablesFrom.
	Variables []AutoinstallVariable `json:"variables,omitempty"`

	// VariablesFrom bulk-imports every key of the referenced Secrets/ConfigMaps
	// as ks_meta variables (like a pod's envFrom). Sources are applied in order;
	// later sources and explicit Variables override earlier keys.
	VariablesFrom []AutoinstallVariableSource `json:"variablesFrom,omitempty"`

	// Earliest is the earliest time at which to schedule the autoinstall action.
	// Defaults to immediate execution.
	Earliest *metav1.Time `json:"earliest,omitempty"`
}

// AutoinstallVariable is one Cobbler ks_meta variable, shaped like a pod env
// var: the value is either literal (Value) or sourced from a Secret/ConfigMap
// key (ValueFrom). Exactly one of Value/ValueFrom is set (webhook-enforced).
type AutoinstallVariable struct {
	// Name is the ks_meta variable key.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Value is a literal value. Mutually exclusive with ValueFrom.
	Value string `json:"value,omitempty"`

	// ValueFrom sources the value from a Secret or ConfigMap key in the same
	// namespace. Mutually exclusive with Value.
	ValueFrom *AutoinstallVariableValueFrom `json:"valueFrom,omitempty"`
}

// AutoinstallVariableValueFrom selects a variable value from a Secret or
// ConfigMap key. Exactly one of the two is set (webhook-enforced).
type AutoinstallVariableValueFrom struct {
	// SecretKeyRef selects a key of a Secret in the same namespace.
	SecretKeyRef *SecretKeyRef `json:"secretKeyRef,omitempty"`
	// ConfigMapKeyRef selects a key of a ConfigMap in the same namespace.
	ConfigMapKeyRef *ConfigMapKeyRef `json:"configMapKeyRef,omitempty"`
}

// AutoinstallVariableSource bulk-imports all keys of a Secret or ConfigMap as
// ks_meta variables. Exactly one of the two is set (webhook-enforced).
type AutoinstallVariableSource struct {
	// SecretRef imports all keys from a Secret in the same namespace.
	SecretRef *LocalObjectRef `json:"secretRef,omitempty"`
	// ConfigMapRef imports all keys from a ConfigMap in the same namespace.
	ConfigMapRef *LocalObjectRef `json:"configMapRef,omitempty"`
	// Prefix is prepended to each imported variable key.
	Prefix string `json:"prefix,omitempty"`
}

type NetworkInterface struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
	MACAddress string `json:"macAddress"`

	IPAddress string `json:"ipAddress,omitempty"`
}

// CustomInfoValue assigns a value to an organization custom info key.
type CustomInfoValue struct {
	// KeyRef references a CustomInfoKey CR in the same namespace.
	// +kubebuilder:validation:Required
	KeyRef LocalObjectRef `json:"keyRef"`

	// Value is the custom info value for the referenced key.
	Value string `json:"value"`
}

// FormulaAssignment enables a Salt formula on a system and supplies its form data.
type FormulaAssignment struct {
	// Name is the Salt formula name. The formula must be installed on the
	// Uyuni server (formula.listFormulas).
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Values is the formula form data: arbitrary nested key/value configuration
	// matching the formula's form definition.
	// +kubebuilder:pruning:PreserveUnknownFields
	Values runtime.RawExtension `json:"values,omitempty"`
}

type SystemSpec struct {
	// +kubebuilder:validation:Required
	// Immutable after creation.
	MinionID string `json:"minionId"`

	// +kubebuilder:validation:Enum=pos;storehub;branchserver
	// SystemType classifies the system role. Optional; omit for generic managed systems.
	SystemType SystemType `json:"type,omitempty"`

	// If true, create an empty system profile in Uyuni before the system
	// boots, so configuration applies on first registration. Requires at
	// least one of: network[].macAddress, hostname.
	PreCreate bool `json:"preCreate,omitempty"`

	// +kubebuilder:default=true
	// If true, schedule a Salt highstate run (system.scheduleApplyHighstate)
	// the first time the system completes registration and is fully
	// reconciled — applying all assigned states (channels, config channels,
	// formulas, etc.) immediately rather than waiting for the next scheduled
	// highstate.
	ApplyHighState bool `json:"applyHighState,omitempty"`

	// Hostname used in the pre-created profile. Defaults to minionId via
	// the SystemDefaulter mutating webhook.
	Hostname string `json:"hostname,omitempty"`

	Network []NetworkInterface `json:"network,omitempty"`

	Description string `json:"description,omitempty"`

	BaseChannelRef    *LocalObjectRef      `json:"baseChannelRef,omitempty"`
	BaseChannelFrom   *ChannelFromProject  `json:"baseChannelFrom,omitempty"`
	ChildChannelRefs  []LocalObjectRef     `json:"childChannelRefs,omitempty"`
	ChildChannelsFrom []ChannelFromProject `json:"childChannelsFrom,omitempty"`

	// CustomInfoValues sets organization-defined custom info key/value pairs on
	// the system. Each entry references a CustomInfoKey CR (which guarantees the
	// key exists in Uyuni before a value is set) and supplies the value.
	CustomInfoValues []CustomInfoValue `json:"customInfoValues,omitempty"`

	// Formulas enables Salt formulas on the system and supplies their form data.
	Formulas []FormulaAssignment `json:"formulas,omitempty"`

	// ProxyRef connects this system through another registered System that is a
	// Uyuni proxy. Clearing it reconnects the system directly to the server.
	ProxyRef *LocalObjectRef `json:"proxyRef,omitempty"`

	// AddOns are additional entitlements granted to the system, e.g.
	// "virtualization_host", "container_build_host", "osimage_build_host",
	// "ansible_control_node", "monitoring_entitled".
	AddOns []string `json:"addOns,omitempty"`

	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// Defaulted by webhook: 24h if PreCreate, 30m otherwise.
	AdoptionTimeout metav1.Duration `json:"adoptionTimeout,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`

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

	// ActiveFormulas is the set of Salt formulas last enabled on the system.
	ActiveFormulas []string `json:"activeFormulas,omitempty"`

	// ProxyServerID is the Uyuni server ID of the proxy the system was last
	// connected through (0 = direct to server).
	ProxyServerID int `json:"proxyServerId,omitempty"`

	// ProxyActionID is the Uyuni action ID of the last changeProxy action.
	ProxyActionID int `json:"proxyActionId,omitempty"`

	// ConfigChannelLabels is the realized ordered config channel subscription
	// (direct refs first, then group-sourced). Used to detect and apply drift.
	ConfigChannelLabels []string `json:"configChannelLabels,omitempty"`

	// GroupNames is the set of Uyuni group names the system was last placed in.
	// Used to compute removals on next reconcile.
	GroupNames []string `json:"groupNames,omitempty"`

	// AutoinstallActionID is the Uyuni action ID of the last scheduled provisioning.
	AutoinstallActionID int `json:"autoinstallActionId,omitempty"`

	// AutoinstallRecordLabel is the Cobbler autoinstall profile label the
	// pre-create system record (system.createSystemRecord) was last created
	// with. Empty until the record is created; used to make record creation
	// idempotent and to detect a changed profile.
	AutoinstallRecordLabel string `json:"autoinstallRecordLabel,omitempty"`

	// AutoinstallStatus reflects the Uyuni-side outcome of the last provisioning action.
	// +kubebuilder:validation:Enum=Scheduled;Completed;Failed
	AutoinstallStatus string `json:"autoinstallStatus,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MinionID",type=string,JSONPath=`.spec.minionId`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
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

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`

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
