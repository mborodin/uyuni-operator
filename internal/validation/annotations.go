package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// StrictBooleanAnnotations validates that any of the listed annotation
// names, if present in the map, have the exact value "true". This is
// intentionally strict: the value "yes" or "1" is rejected so users can't
// trip footgun annotations through casual typing.
//
// Only checks the annotations passed in; ignores unrelated annotations.
func StrictBooleanAnnotations(
	annotations map[string]string,
	names []string,
	basePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	for _, name := range names {
		v, ok := annotations[name]
		if !ok {
			continue
		}
		if v != "true" {
			errs = append(errs, field.Invalid(
				basePath.Key(name), v,
				`must be exactly "true" or absent`))
		}
	}
	return errs
}

// DangerousAnnotations is the canonical set whose presence has
// safety-critical effects (delete behavior, irreversible operations).
var DangerousAnnotations = []string{
	uyuniv1.AnnForceDelete,
}
