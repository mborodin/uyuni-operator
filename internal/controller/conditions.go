package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition type names used across reconcilers. Kept here so they don't
// drift across files.
const (
	condReady                = "Ready"
	condUyuniDrift           = "UyuniDrift"
	condBuildHost            = "BuildHost"
	condPreProvisioned       = "PreProvisioned"
	condAutoinstallScheduled = "AutoinstallScheduled"
	condFormulaValuesDrift   = "FormulaValuesDrift"
	condPackagesSynced       = "PackagesSynced"
)

// setCondition is the generic primitive. setReady and similar are thin
// wrappers so the call sites stay readable.
func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, observedGen int64, reason, message string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGen,
	})
}

func setReady(conds *[]metav1.Condition, observedGen int64, status metav1.ConditionStatus, reason, message string) {
	setCondition(conds, condReady, status, observedGen, reason, message)
}

func setDrift(conds *[]metav1.Condition, observedGen int64, drifted bool, reason, message string) {
	status := metav1.ConditionFalse
	if drifted {
		status = metav1.ConditionTrue
	}
	setCondition(conds, condUyuniDrift, status, observedGen, reason, message)
}

// setPackagesSynced reports whether a SoftwareChannel's repo sync actually
// pulled packages in — distinct from Ready, which only means "the operator
// did what the spec asked" (channel exists, repos associated, schedule set).
// A channel can be Ready=True with PackagesSynced=False if e.g. the
// repository URL is wrong or unreachable.
func setPackagesSynced(conds *[]metav1.Condition, observedGen int64, synced bool, reason, message string) {
	status := metav1.ConditionFalse
	if synced {
		status = metav1.ConditionTrue
	}
	setCondition(conds, condPackagesSynced, status, observedGen, reason, message)
}
