// Package validation contains structural validations shared between admission
// webhooks and (where defense-in-depth requires) controllers. Functions in
// this package have no I/O and don't import sigs.k8s.io/controller-runtime —
// they validate spec shapes only.
//
// Cross-resource validation that needs a Kubernetes client belongs to
// webhook validators; this package is the pure-logic core they call into.
package validation
