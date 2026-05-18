package controller

import "sigs.k8s.io/controller-runtime/pkg/client"

// ensureFinalizer adds current to obj's finalizer list if absent.
// Returns true if a change was made; caller should Update() before continuing.
func ensureFinalizer(obj client.Object, current string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == current {
			return false
		}
	}
	obj.SetFinalizers(append(obj.GetFinalizers(), current))
	return true
}

// containsFinalizer reports whether obj carries the given finalizer.
func containsFinalizer(obj client.Object, current string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == current {
			return true
		}
	}
	return false
}

// removeFinalizer removes current from obj's finalizer list.
func removeFinalizer(obj client.Object, current string) {
	finalizers := obj.GetFinalizers()
	out := finalizers[:0]
	for _, f := range finalizers {
		if f != current {
			out = append(out, f)
		}
	}
	obj.SetFinalizers(out)
}
