package validation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// PromotionPair validates that fromEnv → toEnv is a valid promotion step
// within the project's chain. Assumes the project's environments are
// structurally valid (run EnvChain first).
//
// known/pred describe the project's environment chain: known[label] is
// true for every environment that exists, pred[label] is that
// environment's predecessor (empty for the root environment). The caller
// builds these - CONFIRMED LIVE (VerificationTest promote action
// testing): environments are commonly managed via separate ClmEnvironment
// CRDs now (ContentProject.spec.environments is deprecated in favor of
// that - see contentproject_types.go), so a caller that only reads
// project.Spec.Environments will find every environment "not found" for
// any project using the current recommended pattern. Building known/pred
// is therefore the webhook's job (it can query live ClmEnvironment
// objects), not this package's (no I/O here, see package doc.go).
//
// Returns nil if the pair is valid.
func PromotionPair(
	known map[string]bool,
	pred map[string]string,
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
