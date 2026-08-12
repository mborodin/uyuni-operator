package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MaintenanceScheduleSpec defines a Uyuni maintenance schedule
// (maintenance.createSchedule / updateSchedule / assignScheduleToSystems):
// which systems/groups a set of maintenance-window rules applies to.
//
// CalendarRef is optional. When set, the schedule inherits the referenced
// MaintenanceCalendar's recurring windows and Uyuni restricts scheduled
// actions on assigned systems to those windows. When left empty, the
// schedule carries no calendar at all — Uyuni imposes no time restriction,
// which is the mechanism for stores that run 24/7 with no dedicated
// maintenance window.
type MaintenanceScheduleSpec struct {
	// Name is the maintenance schedule name in Uyuni. Immutable after
	// creation — it is the schedule's identity (not part of Uyuni's
	// updateSchedule details struct).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	Name string `json:"name"`

	// Type is "Single" (the schedule may be assigned to at most one system,
	// no groups) or "Multi" (any number of systems and/or groups). Mutable:
	// Uyuni's updateSchedule accepts a new type.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Single;Multi
	Type string `json:"type"`

	// CalendarRef references a MaintenanceCalendar in the same namespace.
	// Omit it for a schedule with no time restriction (24/7 stores).
	CalendarRef *LocalObjectRef `json:"calendarRef,omitempty"`

	// SystemRefs are System CRs in the same namespace to assign this
	// schedule to.
	SystemRefs []LocalObjectRef `json:"systemRefs,omitempty"`

	// SystemGroupRefs are SystemGroup CRs in the same namespace whose
	// members are assigned this schedule. Uyuni has no group-native
	// assignment call, so the reconciler expands membership to individual
	// system IDs. Only meaningful with type: Multi.
	SystemGroupRefs []LocalObjectRef `json:"systemGroupRefs,omitempty"`

	// RescheduleStrategy controls what happens to actions that fall outside
	// the schedule's windows when it is updated or systems are
	// assigned/retracted: Cancel drops the now-out-of-window actions; Fail
	// rejects the operation and leaves the assignment untouched.
	// +kubebuilder:validation:Enum=Cancel;Fail
	// +kubebuilder:default=Cancel
	RescheduleStrategy string `json:"rescheduleStrategy,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type MaintenanceScheduleStatus struct {
	// UyuniID is the schedule ID assigned by Uyuni.
	UyuniID int `json:"uyuniId,omitempty"`

	// ResolvedSystemIDs is the flattened set of Uyuni server IDs currently
	// assigned to this schedule (direct SystemRefs plus SystemGroupRefs
	// membership expanded). Used to compute additions/removals on drift.
	ResolvedSystemIDs []int `json:"resolvedSystemIds,omitempty"`

	// SystemCount is len(ResolvedSystemIDs), kept as a separate scalar field
	// because kubectl printcolumns can't render a list value directly
	// (same reason SystemGroup carries MemberCount alongside its member list).
	SystemCount int `json:"systemCount,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Calendar",type=string,JSONPath=`.spec.calendarRef.name`
// +kubebuilder:printcolumn:name="Systems",type=integer,JSONPath=`.status.systemCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type MaintenanceSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MaintenanceScheduleSpec   `json:"spec,omitempty"`
	Status            MaintenanceScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MaintenanceScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaintenanceSchedule{}, &MaintenanceScheduleList{})
}
