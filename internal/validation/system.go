package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// SystemFormulas validates a system's formula assignments: each must name a
// formula, and names must be unique.
func SystemFormulas(formulas []uyuniv1.FormulaAssignment, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]bool{}
	for i, f := range formulas {
		p := path.Index(i).Child("name")
		if f.Name == "" {
			errs = append(errs, field.Required(p, "formula name is required"))
			continue
		}
		if seen[f.Name] {
			errs = append(errs, field.Duplicate(p, f.Name))
		}
		seen[f.Name] = true
	}
	return errs
}

// SystemCustomInfoValues validates a system's custom info values: each must
// reference a CustomInfoKey, and keyRefs must be unique.
func SystemCustomInfoValues(values []uyuniv1.CustomInfoValue, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]bool{}
	for i, v := range values {
		p := path.Index(i).Child("keyRef", "name")
		if v.KeyRef.Name == "" {
			errs = append(errs, field.Required(p, "keyRef.name is required"))
			continue
		}
		if seen[v.KeyRef.Name] {
			errs = append(errs, field.Duplicate(p, v.KeyRef.Name))
		}
		seen[v.KeyRef.Name] = true
	}
	return errs
}
