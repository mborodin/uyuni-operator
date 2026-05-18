package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SystemTarget struct {
	SystemRef      *LocalObjectRef `json:"systemRef,omitempty"`
	SystemGroupRef *LocalObjectRef `json:"systemGroupRef,omitempty"`
	ServerIDs      []int           `json:"serverIds,omitempty"`
}

type HighstateSpec struct {
	Test bool `json:"test,omitempty"`
}

type RemoteCommandSpec struct {
	// +kubebuilder:validation:Required
	Command string `json:"command"`

	User  string `json:"user,omitempty"`
	Group string `json:"group,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

type RebootSpec struct {
	DelaySeconds int `json:"delaySeconds,omitempty"`
}

type ApplyPatchesSpec struct {
	// +kubebuilder:validation:Enum=low;moderate;important;critical
	SeverityAtLeast   string   `json:"severityAtLeast,omitempty"`
	IncludeAdvisories []string `json:"includeAdvisories,omitempty"`
}

type ApplyConfigChannelsSpec struct{}

type TaskSpec struct {
	// +kubebuilder:validation:Required
	Target SystemTarget `json:"target"`

	// Exactly one of the following must be set. Webhook enforces.
	Highstate           *HighstateSpec           `json:"highstate,omitempty"`
	RemoteCommand       *RemoteCommandSpec       `json:"remoteCommand,omitempty"`
	Reboot              *RebootSpec              `json:"reboot,omitempty"`
	ApplyPatches        *ApplyPatchesSpec        `json:"applyPatches,omitempty"`
	ApplyConfigChannels *ApplyConfigChannelsSpec `json:"applyConfigChannels,omitempty"`

	NotBefore *metav1.Time `json:"notBefore,omitempty"`

	// +kubebuilder:default="168h"
	TTLAfterFinished metav1.Duration `json:"ttlAfterFinished,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type TaskRunResult struct {
	SystemID int    `json:"systemId"`
	ActionID int    `json:"actionId"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type TaskRun struct {
	Sequence  int   `json:"sequence"`
	ActionIDs []int `json:"actionIds,omitempty"`

	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Mixed
	Phase string `json:"phase"`

	StartedAt     *metav1.Time    `json:"startedAt,omitempty"`
	CompletedAt   *metav1.Time    `json:"completedAt,omitempty"`
	Results       []TaskRunResult `json:"results,omitempty"`
	FailureReason string          `json:"failureReason,omitempty"`
	Trigger       string          `json:"trigger,omitempty"`
}

type TaskStatus struct {
	Runs []TaskRun `json:"runs,omitempty"`

	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Mixed
	Phase string `json:"phase,omitempty"`

	ResolvedSystemIDs  []int              `json:"resolvedSystemIds,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runs",type=integer,JSONPath=`.status.runs[*].sequence`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TaskSpec   `json:"spec,omitempty"`
	Status            TaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
