package controller

import "sigs.k8s.io/controller-runtime/pkg/client"

// legacyAnnotationMap maps deprecated uyuni.io/* annotation keys to their
// current uyuni.uyuni-project.org/* equivalents. Checked once per reconcile
// via migrateAnnotations so live CRs are upgraded transparently.
// Remove after the post-v0.x migration window.
var legacyAnnotationMap = map[string]string{
	"uyuni.io/force-delete":   "uyuni.uyuni-project.org/force-delete",
	"uyuni.io/rerun":          "uyuni.uyuni-project.org/rerun",
	"uyuni.io/build-now":      "uyuni.uyuni-project.org/build-now",
	"uyuni.io/sync-now":       "uyuni.uyuni-project.org/sync-now",
	"uyuni.io/build-version":  "uyuni.uyuni-project.org/build-version",
	"uyuni.io/reinstall-now":  "uyuni.uyuni-project.org/reinstall-now",
}

// migrateAnnotations promotes any legacy uyuni.io/* annotations to their
// current uyuni.uyuni-project.org/* equivalents. Returns true if any
// annotation was renamed; the caller must Update() the object to persist.
func migrateAnnotations(obj client.Object) bool {
	anns := obj.GetAnnotations()
	if len(anns) == 0 {
		return false
	}
	changed := false
	for old, current := range legacyAnnotationMap {
		if v, ok := anns[old]; ok {
			delete(anns, old)
			anns[current] = v
			changed = true
		}
	}
	if changed {
		obj.SetAnnotations(anns)
	}
	return changed
}
