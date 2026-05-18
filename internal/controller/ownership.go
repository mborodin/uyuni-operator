package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// reconcileProjectOwnership ensures the dependent has owner refs that
// match the set of ContentProject names it references. Adds missing,
// removes stale. Idempotent.
//
// Owner refs are set with:
//   - Controller=false (we don't manage the dependent's lifecycle)
//   - BlockOwnerDeletion=true (project's finalizer blocks until the
//     dependent's own finalizer completes — preserves Uyuni-side
//     ordering: clean up subscriptions before removing the project)
//
// Note: BlockOwnerDeletion=true requires `update` on
// /finalizers subresource of the owner kind. Make sure RBAC grants this.
func reconcileProjectOwnership(ctx context.Context, c client.Client, dependent client.Object, wantedProjectNames map[string]bool) error {
	owners := dependent.GetOwnerReferences()

	// Index current ContentProject owners by name.
	currentProjectOwners := map[string]int{}
	for i, o := range owners {
		if o.APIVersion == uyuniv1.GroupVersion.String() && o.Kind == "ContentProject" {
			currentProjectOwners[o.Name] = i
		}
	}

	changed := false

	// Add missing.
	for name := range wantedProjectNames {
		if _, ok := currentProjectOwners[name]; ok {
			continue
		}
		var cp uyuniv1.ContentProject
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: dependent.GetNamespace(), Name: name,
		}, &cp); err != nil {
			if client.IgnoreNotFound(err) == nil {
				// Project doesn't exist; resolver reports this as a hard
				// error elsewhere. Don't fail ownership reconciliation —
				// just skip the missing owner.
				continue
			}
			return err
		}
		blockOwner := true
		owners = append(owners, metav1.OwnerReference{
			APIVersion:         uyuniv1.GroupVersion.String(),
			Kind:               "ContentProject",
			Name:               cp.Name,
			UID:                cp.UID,
			BlockOwnerDeletion: &blockOwner,
		})
		changed = true
	}

	// Prune stale ContentProject owners.
	pruned := owners[:0]
	for _, o := range owners {
		if o.APIVersion == uyuniv1.GroupVersion.String() && o.Kind == "ContentProject" {
			if !wantedProjectNames[o.Name] {
				changed = true
				continue
			}
		}
		pruned = append(pruned, o)
	}
	owners = pruned

	if !changed {
		return nil
	}
	dependent.SetOwnerReferences(owners)
	return c.Update(ctx, dependent)
}

// isOwnedBy returns true if dependent has an owner reference with the
// given UID. UID-based check avoids spurious matches if the owner CR is
// recreated with the same name.
func isOwnedBy(dependent, owner client.Object) bool {
	for _, o := range dependent.GetOwnerReferences() {
		if o.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

// projectOwnersFromActivationKey returns the set of ContentProject names
// referenced by an ActivationKey's *From fields.
func projectOwnersFromActivationKey(ak *uyuniv1.ActivationKey) map[string]bool {
	out := map[string]bool{}
	if ak.Spec.BaseChannelFrom != nil {
		out[ak.Spec.BaseChannelFrom.ContentProjectRef.Name] = true
	}
	for _, c := range ak.Spec.ChildChannelsFrom {
		out[c.ContentProjectRef.Name] = true
	}
	return out
}

// projectOwnersFromSystem returns the set of ContentProject names
// referenced by a System's *From fields.
func projectOwnersFromSystem(sys *uyuniv1.System) map[string]bool {
	out := map[string]bool{}
	if sys.Spec.BaseChannelFrom != nil {
		out[sys.Spec.BaseChannelFrom.ContentProjectRef.Name] = true
	}
	for _, c := range sys.Spec.ChildChannelsFrom {
		out[c.ContentProjectRef.Name] = true
	}
	return out
}

// refsActivationKeyProject returns true if the AK references the named
// project via either *From field. Used by watch mappers.
func refsActivationKeyProject(ak *uyuniv1.ActivationKey, projectName string) bool {
	if ak.Spec.BaseChannelFrom != nil && ak.Spec.BaseChannelFrom.ContentProjectRef.Name == projectName {
		return true
	}
	for _, c := range ak.Spec.ChildChannelsFrom {
		if c.ContentProjectRef.Name == projectName {
			return true
		}
	}
	return false
}

func refsSystemProject(sys *uyuniv1.System, projectName string) bool {
	if sys.Spec.BaseChannelFrom != nil && sys.Spec.BaseChannelFrom.ContentProjectRef.Name == projectName {
		return true
	}
	for _, c := range sys.Spec.ChildChannelsFrom {
		if c.ContentProjectRef.Name == projectName {
			return true
		}
	}
	return false
}
