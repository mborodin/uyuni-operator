package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// TaskKindCount returns the number of task-kind discriminator fields set
// on a TaskSpec. Webhook validators check this is exactly 1; reconciler
// uses it indirectly via TaskSpec.
func TaskKindCount(s *uyuniv1.TaskSpec) int {
	n := 0
	if s.Highstate != nil {
		n++
	}
	if s.RemoteCommand != nil {
		n++
	}
	if s.Reboot != nil {
		n++
	}
	if s.ApplyPatches != nil {
		n++
	}
	if s.ApplyConfigChannels != nil {
		n++
	}
	return n
}

// TaskTargetCount returns the number of target-style discriminator fields
// set on a SystemTarget.
func TaskTargetCount(t *uyuniv1.SystemTarget) int {
	n := 0
	if t.SystemRef != nil {
		n++
	}
	if t.SystemGroupRef != nil {
		n++
	}
	if len(t.ServerIDs) > 0 {
		n++
	}
	return n
}

// TaskSpec validates the full TaskSpec at admission time. Per-kind specifics
// (timeout bounds, required command, etc.) are checked here too.
func TaskSpec(s *uyuniv1.TaskSpec, basePath *field.Path) field.ErrorList {
	var errs field.ErrorList

	switch TaskKindCount(s) {
	case 0:
		errs = append(errs, field.Required(basePath,
			"one of highstate, remoteCommand, reboot, applyPatches, applyConfigChannels is required"))
	case 1:
		// ok
	default:
		errs = append(errs, field.Forbidden(basePath,
			"exactly one task kind must be specified"))
	}

	switch TaskTargetCount(&s.Target) {
	case 0:
		errs = append(errs, field.Required(basePath.Child("target"),
			"one of systemRef, systemGroupRef, serverIds is required"))
	case 1:
		// ok
	default:
		errs = append(errs, field.Forbidden(basePath.Child("target"),
			"exactly one target style is required"))
	}

	if s.RemoteCommand != nil {
		if s.RemoteCommand.TimeoutSeconds < 0 {
			errs = append(errs, field.Invalid(
				basePath.Child("remoteCommand", "timeoutSeconds"),
				s.RemoteCommand.TimeoutSeconds, "must be non-negative"))
		}
		if s.RemoteCommand.Command == "" {
			errs = append(errs, field.Required(
				basePath.Child("remoteCommand", "command"), ""))
		}
	}

	return errs
}
