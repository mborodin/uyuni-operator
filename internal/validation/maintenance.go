package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// MaintenanceCalendarSourceMutex validates that exactly one of ical/url is
// set on a MaintenanceCalendar — Uyuni's createCalendar and
// createCalendarWithUrl are separate calls; the CR must pick one source.
func MaintenanceCalendarSourceMutex(ical, url string, basePath *field.Path) field.ErrorList {
	switch {
	case ical != "" && url != "":
		return field.ErrorList{field.Forbidden(basePath,
			"ical and url are mutually exclusive")}
	case ical == "" && url == "":
		return field.ErrorList{field.Required(basePath,
			"one of ical or url is required")}
	default:
		return nil
	}
}

// MaintenanceScheduleTargetCount validates the type/target-cardinality rule
// Uyuni enforces on maintenance schedules: a "Single" schedule may be
// assigned to at most one system and no groups at all; "Multi" allows any
// combination. Runs on every create/update, so a Multi→Single edit that
// still lists multiple targets is rejected at admission without needing a
// separate type-immutability rule.
func MaintenanceScheduleTargetCount(
	schedType string,
	systemRefs []uyuniv1.LocalObjectRef,
	systemGroupRefs []uyuniv1.LocalObjectRef,
	basePath *field.Path,
) field.ErrorList {
	if schedType != "Single" {
		return nil
	}
	var errs field.ErrorList
	if len(systemGroupRefs) > 0 {
		errs = append(errs, field.Forbidden(basePath.Child("systemGroupRefs"),
			"type: Single schedules cannot target system groups"))
	}
	if len(systemRefs) > 1 {
		errs = append(errs, field.Forbidden(basePath.Child("systemRefs"),
			"type: Single schedules can target at most one system"))
	}
	return errs
}
