package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PackageCleanupPolicySpec defines a periodic sweep that finds packages no
// longer associated with any software channel and, after re-verifying each
// one is still unreferenced by any patch/errata and not installed on any
// system, removes it from Uyuni (packages.removePackage) to free storage.
//
// Unlike ImageProfile's retention (which prunes Kubernetes-side ImageBuild
// records), there is no per-package Kubernetes object here — packages exist
// only in Uyuni. This policy drives a periodic scan via Uyuni's own API
// (channel.software.listPackagesWithoutChannel), not a reactive reconcile of
// child CRs.
type PackageCleanupPolicySpec struct {
	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`

	// MinOrphanAgeDays is the minimum time, in days, a package record must
	// have existed (per Uyuni's last_modified date) before it's eligible for
	// deletion — a grace period so a package dropped from a channel moments
	// ago isn't swept up immediately.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=7
	MinOrphanAgeDays int `json:"minOrphanAgeDays,omitempty"`

	// DryRun, when true, only reports what would be deleted in status —
	// nothing is actually removed from Uyuni. Defaults to true: a policy
	// must be explicitly flipped to dryRun: false to ever delete anything.
	// +kubebuilder:default=true
	DryRun bool `json:"dryRun,omitempty"`

	// MaxDeletePerRun caps how many packages a single reconcile batch
	// deletes. While more eligible candidates remain, the controller
	// requeues after BatchIntervalSeconds to process the next batch — it
	// does not wait for the idle periodic interval until the backlog is
	// fully drained.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=500
	MaxDeletePerRun int `json:"maxDeletePerRun,omitempty"`

	// BatchIntervalSeconds is the delay between consecutive batches while
	// candidates remain, giving the Uyuni server breathing room between
	// bursts of API calls.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=60
	BatchIntervalSeconds int `json:"batchIntervalSeconds,omitempty"`
}

// PackageCleanupPolicyStatus reports the outcome of the most recent batch,
// for auditability — every run's counts are visible without reading logs.
type PackageCleanupPolicyStatus struct {
	// LastRunTime is when the most recent batch completed.
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// CandidatesFound is the total orphaned-package count from Uyuni
	// (channel.software.listPackagesWithoutChannel) as of the most recent
	// batch, before any per-package filtering.
	CandidatesFound int `json:"candidatesFound,omitempty"`

	// SkippedTooRecent is how many candidates were excluded for not yet
	// meeting MinOrphanAgeDays.
	SkippedTooRecent int `json:"skippedTooRecent,omitempty"`

	// SkippedErrataLinked is how many candidates were excluded because
	// packages.listProvidingErrata still returned at least one entry.
	SkippedErrataLinked int `json:"skippedErrataLinked,omitempty"`

	// SkippedInstalled is how many candidates were excluded because
	// system.listSystemsWithPackage still returned at least one system.
	SkippedInstalled int `json:"skippedInstalled,omitempty"`

	// DeletedThisRun is how many packages were actually removed in the most
	// recent batch. Always 0 while DryRun is true.
	DeletedThisRun int `json:"deletedThisRun,omitempty"`

	// RemainingCandidates is how many eligible candidates were left
	// unprocessed after this batch (bounded by MaxDeletePerRun), driving
	// whether the next reconcile is a follow-up batch or the idle wait.
	RemainingCandidates int `json:"remainingCandidates,omitempty"`

	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DryRun",type=boolean,JSONPath=`.spec.dryRun`
// +kubebuilder:printcolumn:name="Candidates",type=integer,JSONPath=`.status.candidatesFound`
// +kubebuilder:printcolumn:name="Deleted",type=integer,JSONPath=`.status.deletedThisRun`
// +kubebuilder:printcolumn:name="Remaining",type=integer,JSONPath=`.status.remainingCandidates`
// +kubebuilder:printcolumn:name="LastRun",type=date,JSONPath=`.status.lastRunTime`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type PackageCleanupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PackageCleanupPolicySpec   `json:"spec,omitempty"`
	Status            PackageCleanupPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PackageCleanupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PackageCleanupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PackageCleanupPolicy{}, &PackageCleanupPolicyList{})
}
