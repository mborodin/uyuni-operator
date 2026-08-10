package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MaintenanceCalendarSpec defines a reusable Uyuni maintenance calendar
// (maintenance.createCalendar / createCalendarWithUrl): a set of recurring
// maintenance windows expressed as RFC5545 iCal data, either supplied
// inline or fetched by Uyuni from a URL. One calendar can be referenced by
// many MaintenanceSchedules.
type MaintenanceCalendarSpec struct {
	// Label is the maintenance calendar label in Uyuni. Immutable after
	// creation — it is the calendar's identity (not part of Uyuni's
	// updateCalendar details struct).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	Label string `json:"label"`

	// ICal is inline RFC5545 calendar data. Mutually exclusive with URL;
	// exactly one of the two is required.
	ICal string `json:"ical,omitempty"`

	// URL is fetched by Uyuni to obtain the calendar data. Mutually
	// exclusive with ICal; exactly one of the two is required. Refresh with
	// the uyuni.uyuni-project.org/refresh-now annotation.
	URL string `json:"url,omitempty"`

	// RescheduleStrategy controls what happens to actions that fall outside
	// the recomputed windows when the calendar is updated, refreshed, or
	// deleted: Cancel drops the now-out-of-window actions; Fail rejects the
	// operation and leaves the calendar untouched.
	// +kubebuilder:validation:Enum=Cancel;Fail
	// +kubebuilder:default=Cancel
	RescheduleStrategy string `json:"rescheduleStrategy,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type MaintenanceCalendarStatus struct {
	// UyuniID is the calendar ID assigned by Uyuni.
	UyuniID int `json:"uyuniId,omitempty"`

	// LastRefreshedTime records when a URL-backed calendar was last
	// refreshed via the refresh-now annotation.
	LastRefreshedTime *metav1.Time `json:"lastRefreshedTime,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="UyuniID",type=integer,JSONPath=`.status.uyuniId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type MaintenanceCalendar struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MaintenanceCalendarSpec   `json:"spec,omitempty"`
	Status            MaintenanceCalendarStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MaintenanceCalendarList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceCalendar `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaintenanceCalendar{}, &MaintenanceCalendarList{})
}
