package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ActivationKeySpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	// Immutable after creation. Uyuni prefixes with org ID automatically.
	Key string `json:"key"`

	Description string `json:"description,omitempty"`

	// Direct reference. Mutually exclusive with BaseChannelFrom.
	BaseChannelRef *LocalObjectRef `json:"baseChannelRef,omitempty"`

	// Project-environment reference. Mutually exclusive with BaseChannelRef.
	// Webhook enforces; reconciler has a defensive backstop.
	BaseChannelFrom *ChannelFromProject `json:"baseChannelFrom,omitempty"`

	ChildChannelRefs []LocalObjectRef `json:"childChannelRefs,omitempty"`

	// Mutually exclusive with ChildChannelRefs.
	ChildChannelsFrom []ChannelFromProject `json:"childChannelsFrom,omitempty"`

	SystemGroupRefs   []LocalObjectRef `json:"systemGroupRefs,omitempty"`
	ConfigChannelRefs []LocalObjectRef `json:"configChannelRefs,omitempty"`

	Entitlements []string `json:"entitlements,omitempty"`
	Packages     []string `json:"packages,omitempty"`

	// +kubebuilder:default=0
	UsageLimit       int  `json:"usageLimit,omitempty"`
	UniversalDefault bool `json:"universalDefault,omitempty"`

	// +kubebuilder:validation:Enum=default;ssh-push;ssh-push-tunnel
	// +kubebuilder:default=default
	ContactMethod string `json:"contactMethod,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type ActivationKeyStatus struct {
	UyuniKey             string             `json:"uyuniKey,omitempty"`
	ActivatedSystemCount int                `json:"activatedSystemCount,omitempty"`
	ObservedGeneration   int64              `json:"observedGeneration,omitempty"`
	Conditions           []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="UyuniKey",type=string,JSONPath=`.status.uyuniKey`
// +kubebuilder:printcolumn:name="Systems",type=integer,JSONPath=`.status.activatedSystemCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ActivationKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ActivationKeySpec   `json:"spec,omitempty"`
	Status            ActivationKeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ActivationKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActivationKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActivationKey{}, &ActivationKeyList{})
}
