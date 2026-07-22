package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	storeClaimGroup    = "nexa.ahold-delhaize.com"
	storeClaimVersion  = "v1alpha1"
	storeClaimKind     = "StoreClaim"
	storeHubClaimKind  = "StoreHubClaim"
	systemKind         = "System"
)

type StoreClaimReconciler struct {
	client.Client
	previousSpecs map[string]map[string]interface{}
}

// +kubebuilder:rbac:groups=nexa.ahold-delhaize.com,resources=storeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=nexa.ahold-delhaize.com,resources=storehubclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=kubernetes.crossplane.io,resources=objects,verbs=get;list;watch;delete

func (r *StoreClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.previousSpecs == nil {
		r.previousSpecs = make(map[string]map[string]interface{})
	}

	// Get the StoreClaim resource
	storeClaim := &unstructured.Unstructured{}
	storeClaim.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   storeClaimGroup,
		Version: storeClaimVersion,
		Kind:    storeClaimKind,
	})

	if err := r.Get(ctx, req.NamespacedName, storeClaim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	namespace := storeClaim.GetNamespace()
	name := storeClaim.GetName()
	resourceKey := fmt.Sprintf("%s/%s", namespace, name)

	spec, found, err := unstructured.NestedMap(storeClaim.Object, "spec")
	if err != nil || !found {
		return ctrl.Result{}, err
	}

	// Check for removed systems
	r.cleanupRemovedSystems(ctx, namespace, name, spec)

	// Check for removed storeHubs
	r.cleanupRemovedStoreHubs(ctx, namespace, name, spec)

	// Store current spec for next comparison
	r.previousSpecs[resourceKey] = spec

	return ctrl.Result{}, nil
}

func (r *StoreClaimReconciler) cleanupRemovedSystems(ctx context.Context, namespace, storeClaimName string, currentSpec map[string]interface{}) {
	currentSystems := make(map[string]bool)
	if systemsArray, ok := currentSpec["systems"].([]interface{}); ok {
		for _, sys := range systemsArray {
			if sysMap, ok := sys.(map[string]interface{}); ok {
				if name, ok := sysMap["name"].(string); ok {
					currentSystems[name] = true
				}
			}
		}
	}

	// Get XStore name (used in Object names)
	xstoreName := fmt.Sprintf("%s-sc7t4", storeClaimName) // Format: storeclaim-name + random suffix

	// Delete System Objects for removed systems
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubernetes.crossplane.io",
		Version: "v1alpha2",
		Kind:    "ObjectList",
	})

	if err := r.List(ctx, list, &client.ListOptions{
		Namespace: namespace,
	}); err != nil {
		return
	}

	for _, item := range list.Items {
		if item.GetKind() != "Object" {
			continue
		}

		// Check if this Object is a System created by the StoreClaim Composition
		manifest, _, _ := unstructured.NestedMap(item.Object, "spec", "forProvider", "manifest")
		if manifest == nil {
			continue
		}

		kind, _, _ := unstructured.NestedString(manifest, "kind")
		if kind != systemKind {
			continue
		}

		objName, _, _ := unstructured.NestedString(manifest, "metadata", "name")
		if objName == "" {
			continue
		}

		// Check if this system is still in the current spec
		// System names are formatted as: xstore-name-system-name
		for sysName := range currentSystems {
			if objName == fmt.Sprintf("%s-%s", xstoreName, sysName) {
				// System still exists in spec, don't delete
				return
			}
		}

		// System was removed from spec, delete the Object
		if err := r.Delete(ctx, &item); err != nil {
			// Log but don't fail
			continue
		}
	}
}

func (r *StoreClaimReconciler) cleanupRemovedStoreHubs(ctx context.Context, namespace, storeClaimName string, currentSpec map[string]interface{}) {
	currentStoreHubs := make(map[string]bool)
	if storeHubMap, ok := currentSpec["storeHub"].(map[string]interface{}); ok {
		for _, hub := range storeHubMap {
			if hubMap, ok := hub.(map[string]interface{}); ok {
				if name, ok := hubMap["name"].(string); ok {
					currentStoreHubs[name] = true
				}
			}
		}
	}

	// Delete StoreHubClaims for removed storeHubs
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   storeClaimGroup,
		Version: storeClaimVersion,
		Kind:    "StoreHubClaimList",
	})

	if err := r.List(ctx, list, &client.ListOptions{
		Namespace: namespace,
	}); err != nil {
		return
	}

	for _, item := range list.Items {
		if item.GetKind() != storeHubClaimKind {
			continue
		}

		// Check if this StoreHubClaim was created by this StoreClaim
		spec, _, _ := unstructured.NestedMap(item.Object, "spec")
		if spec == nil {
			continue
		}

		storeRef, _, _ := unstructured.NestedString(spec, "store")
		if storeRef == "" || len(storeRef) < len(storeClaimName) {
			continue
		}

		// Verify it belongs to this StoreClaim
		if storeRef[:len(storeClaimName)] != storeClaimName {
			continue
		}

		// Check if this StoreHub is still in the current spec
		shcName := item.GetName()
		if currentStoreHubs[shcName] {
			continue
		}

		// StoreHub was removed from spec, delete the StoreHubClaim
		if err := r.Delete(ctx, &item); err != nil {
			// Log but don't fail
			continue
		}
	}
}

func (r *StoreClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.previousSpecs = make(map[string]map[string]interface{})

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   storeClaimGroup,
		Version: storeClaimVersion,
		Kind:    storeClaimKind,
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			// Only watch StoreClaim resources
			return object.GetObjectKind().GroupVersionKind().Kind == storeClaimKind &&
				object.GetObjectKind().GroupVersionKind().Group == storeClaimGroup
		})).
		Complete(r)
}
