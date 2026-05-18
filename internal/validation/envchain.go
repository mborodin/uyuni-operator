package validation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// EnvChain validates a ContentProject environment list. Checks:
//   - non-empty
//   - unique labels
//   - exactly one root (predecessor=="")
//   - every non-root predecessor refers to a declared label
//   - acyclic
//
// All applicable errors are collected and returned together so users see
// the full set in one round-trip. Cycle detection only runs on otherwise-
// well-formed input because it assumes a clean graph.
func EnvChain(envs []uyuniv1.ProjectEnvironment, basePath *field.Path) field.ErrorList {
	var errs field.ErrorList

	if len(envs) == 0 {
		errs = append(errs, field.Required(basePath, "at least one environment required"))
		return errs
	}

	seen := make(map[string]bool, len(envs))
	roots := 0
	for i, e := range envs {
		path := basePath.Index(i)
		if seen[e.Label] {
			errs = append(errs, field.Duplicate(path.Child("label"), e.Label))
		}
		seen[e.Label] = true
		if e.Predecessor == "" {
			roots++
		}
	}

	switch roots {
	case 0:
		errs = append(errs, field.Invalid(basePath, "",
			"exactly one root environment (predecessor=\"\") required, got 0"))
	case 1:
		// ok
	default:
		errs = append(errs, field.Invalid(basePath, "",
			fmt.Sprintf("exactly one root environment (predecessor=\"\") required, got %d", roots)))
	}

	for i, e := range envs {
		if e.Predecessor != "" && !seen[e.Predecessor] {
			errs = append(errs, field.NotFound(
				basePath.Index(i).Child("predecessor"), e.Predecessor))
		}
	}

	if len(errs) == 0 && !isAcyclic(envs) {
		errs = append(errs, field.Invalid(basePath, "",
			"environment chain contains a cycle"))
	}

	return errs
}

func isAcyclic(envs []uyuniv1.ProjectEnvironment) bool {
	pred := make(map[string]string, len(envs))
	for _, e := range envs {
		pred[e.Label] = e.Predecessor
	}
	for _, e := range envs {
		seen := map[string]bool{e.Label: true}
		cursor := e.Predecessor
		for cursor != "" {
			if seen[cursor] {
				return false
			}
			seen[cursor] = true
			cursor = pred[cursor]
		}
	}
	return true
}

// ChainOrder returns environments topologically sorted by predecessor.
// Only call this AFTER EnvChain has validated the input.
func ChainOrder(envs []uyuniv1.ProjectEnvironment) []uyuniv1.ProjectEnvironment {
	byPredecessor := make(map[string]uyuniv1.ProjectEnvironment, len(envs))
	for _, e := range envs {
		byPredecessor[e.Predecessor] = e
	}
	out := make([]uyuniv1.ProjectEnvironment, 0, len(envs))
	cursor := ""
	for {
		next, ok := byPredecessor[cursor]
		if !ok {
			break
		}
		out = append(out, next)
		cursor = next.Label
	}
	return out
}
