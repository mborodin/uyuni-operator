package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// AutoinstallVariables validates spec.autoinstall variables and variablesFrom:
// each variable needs a unique name, must not set both value and valueFrom, and
// a valueFrom must select exactly one of secretKeyRef/configMapKeyRef; each
// variablesFrom source must set exactly one of secretRef/configMapRef. An empty
// literal value (neither value nor valueFrom) is allowed. path points at
// spec.autoinstall.
func AutoinstallVariables(ai *uyuniv1.AutoinstallSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if ai == nil {
		return errs
	}
	seen := map[string]bool{}
	for i, v := range ai.Variables {
		p := path.Child("variables").Index(i)
		if v.Name == "" {
			errs = append(errs, field.Required(p.Child("name"), "name is required"))
		} else if seen[v.Name] {
			errs = append(errs, field.Duplicate(p.Child("name"), v.Name))
		}
		seen[v.Name] = true

		if v.Value != "" && v.ValueFrom != nil {
			errs = append(errs, field.Invalid(p, v.Name,
				"value and valueFrom are mutually exclusive"))
		}
		if v.ValueFrom != nil {
			hasSecret := v.ValueFrom.SecretKeyRef != nil
			hasCM := v.ValueFrom.ConfigMapKeyRef != nil
			if hasSecret == hasCM {
				errs = append(errs, field.Invalid(p.Child("valueFrom"), v.Name,
					"exactly one of secretKeyRef or configMapKeyRef must be set"))
			}
		}
	}
	for i, src := range ai.VariablesFrom {
		p := path.Child("variablesFrom").Index(i)
		hasSecret := src.SecretRef != nil
		hasCM := src.ConfigMapRef != nil
		if hasSecret == hasCM {
			errs = append(errs, field.Invalid(p, "",
				"exactly one of secretRef or configMapRef must be set"))
		}
	}
	return errs
}

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
