package validation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// PromotionPair validates that fromEnv → toEnv is a valid promotion step
// within the project's chain. Assumes the project's environments are
// structurally valid (run EnvChain first).
//
// Returns nil if the pair is valid.
func PromotionPair(
	project *uyuniv1.ContentProject,
	fromEnv, toEnv string,
	fromPath, toPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	if fromEnv == "" {
		errs = append(errs, field.Required(fromPath, ""))
	}
	if toEnv == "" {
		errs = append(errs, field.Required(toPath, ""))
	}
	if fromEnv != "" && fromEnv == toEnv {
		errs = append(errs, field.Invalid(toPath, toEnv,
			"toEnvironment must differ from fromEnvironment"))
	}
	if len(errs) > 0 {
		return errs
	}

	pred := make(map[string]string, len(project.Spec.Environments))
	known := make(map[string]bool, len(project.Spec.Environments))
	for _, e := range project.Spec.Environments {
		pred[e.Label] = e.Predecessor
		known[e.Label] = true
	}

	if !known[fromEnv] {
		errs = append(errs, field.NotFound(fromPath, fromEnv))
	}
	toPred, toExists := pred[toEnv]
	if !toExists {
		errs = append(errs, field.NotFound(toPath, toEnv))
	} else if known[fromEnv] && toPred != fromEnv {
		errs = append(errs, field.Invalid(toPath, toEnv,
			fmt.Sprintf("not the successor of %q in project's chain (its predecessor is %q)",
				fromEnv, toPred)))
	}
	return errs
}
