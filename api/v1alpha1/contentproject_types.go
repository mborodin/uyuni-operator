package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ProjectEnvironment struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Label string `json:"label"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	// Predecessor in the promotion chain. Empty for root.
	Predecessor string `json:"predecessor,omitempty"`
}

type FilterCriteria struct {
	// +kubebuilder:validation:Enum=name;nevr;nevra;provides_name;synopsis;advisory_type;advisory_name
	Field string `json:"field"`

	// +kubebuilder:validation:Enum=equals;matches;contains;greater;lower
	Matcher string `json:"matcher"`

	Value string `json:"value"`
}

type ProjectFilter struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Enum=package;errata;module;appstream;ptf
	Type string `json:"type"`

	// +kubebuilder:validation:Required
	Criteria FilterCriteria `json:"criteria"`

	// +kubebuilder:validation:Enum=allow;deny
	// +kubebuilder:default=deny
	Rule string `json:"rule"`
}

type ProjectBuildPolicy struct {
	// Trigger a build when source channels change.
	AutoBuildSources bool `json:"autoBuildSources,omitempty"`

	// Cron expression for periodic builds. Standard 5-field format.
	Schedule string `json:"schedule,omitempty"`

	// +kubebuilder:default="automated build by uyuni-operator"
	Message string `json:"message,omitempty"`
}

type ContentProjectSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	// Immutable after creation.
	Label string `json:"label"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	SourceRefs []LocalObjectRef `json:"sourceRefs,omitempty"`

	// Environments are now managed via separate ClmEnvironment CRDs
	Environments []ProjectEnvironment `json:"environments,omitempty"`

	Filters []ProjectFilter `json:"filters,omitempty"`

	Build ProjectBuildPolicy `json:"build,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type EnvironmentState struct {
	Label           string       `json:"label"`
	Name            string       `json:"name,omitempty"`
	BuiltVersion    int          `json:"builtVersion,omitempty"`
	BuiltAt         *metav1.Time `json:"builtAt,omitempty"`
	DerivedChannels []string     `json:"derivedChannels,omitempty"`
}

type ContentProjectStatus struct {
	UyuniID                    int                `json:"uyuniId,omitempty"`
	AttachedSources            []string           `json:"attachedSources,omitempty"`
	FilterIDs                  map[string]int     `json:"filterIds,omitempty"`
	EnvironmentStates          []EnvironmentState `json:"environmentStates,omitempty"`
	LastBuildActionID          int                `json:"lastBuildActionId,omitempty"`
	LastBuildStartedAt         *metav1.Time       `json:"lastBuildStartedAt,omitempty"`
	LastBuildSourceFingerprint string             `json:"lastBuildSourceFingerprint,omitempty"`

	// +kubebuilder:validation:Enum=Idle;Building;Failed
	BuildStatus string `json:"buildStatus,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Build",type=string,JSONPath=`.status.buildStatus`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type ContentProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContentProjectSpec   `json:"spec,omitempty"`
	Status            ContentProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ContentProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContentProject `json:"items"`
}

type ContentProjectPromotionSpec struct {
	// +kubebuilder:validation:Required
	ProjectRef LocalObjectRef `json:"projectRef"`

	// +kubebuilder:validation:Required
	FromEnvironment string `json:"fromEnvironment"`

	// +kubebuilder:validation:Required
	ToEnvironment string `json:"toEnvironment"`

	// Optional sanity check: fail if source env's current built version
	// differs from this value.
	RequireSourceVersion int `json:"requireSourceVersion,omitempty"`

	// Run no earlier than this. Useful for change-window scheduling.
	NotBefore *metav1.Time `json:"notBefore,omitempty"`

	// +kubebuilder:default="168h"
	TTLAfterFinished metav1.Duration `json:"ttlAfterFinished,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ContentProjectPromotionStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	Phase string `json:"phase,omitempty"`

	PromotedVersion    int                `json:"promotedVersion,omitempty"`
	StartedAt          *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt        *metav1.Time       `json:"completedAt,omitempty"`
	FailureReason      string             `json:"failureReason,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.fromEnvironment`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.toEnvironment`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.promotedVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ContentProjectPromotion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContentProjectPromotionSpec   `json:"spec,omitempty"`
	Status            ContentProjectPromotionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ContentProjectPromotionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContentProjectPromotion `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&ContentProject{}, &ContentProjectList{},
		&ContentProjectPromotion{}, &ContentProjectPromotionList{},
	)
}
