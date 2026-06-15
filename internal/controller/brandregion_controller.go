package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type BrandRegionReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=brandregions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=brandregions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=brandregions/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=uyuniproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=clmenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch

func (r *BrandRegionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var br uyuniv1.BrandRegion
	if err := r.Get(ctx, req.NamespacedName, &br); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !br.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &br)
	}
	if ensureFinalizer(&br, brFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &br)
	}

	// 1. UyuniProvider (cluster-scoped — no owner refs, managed via finalizer).
	if err := r.reconcileProvider(ctx, &br); err != nil {
		return r.fail(ctx, &br, "ProviderFailed", err)
	}
	// org and child resources use the provider named after the BrandRegion.
	providerRef := uyuniv1.LocalObjectRef{Name: br.Name}
	orgRef := &uyuniv1.LocalObjectRef{Name: br.Name}

	// 2. Organization.
	if err := r.reconcileOrganization(ctx, &br, providerRef); err != nil {
		return r.fail(ctx, &br, "OrganizationFailed", err)
	}

	// 3. Repositories.
	managedRepos := make([]string, 0, len(br.Spec.Repositories))
	for _, repo := range br.Spec.Repositories {
		if err := r.reconcileRepository(ctx, &br, repo, orgRef); err != nil {
			return r.fail(ctx, &br, "RepositoryFailed", fmt.Errorf("repository %q: %w", repo.Name, err))
		}
		managedRepos = append(managedRepos, repo.Name)
	}

	// 4. SoftwareChannels.
	managedSC := make([]string, 0, len(br.Spec.SoftwareChannels))
	for _, sc := range br.Spec.SoftwareChannels {
		if err := r.reconcileSoftwareChannel(ctx, &br, sc, orgRef); err != nil {
			return r.fail(ctx, &br, "SoftwareChannelFailed", fmt.Errorf("softwareChannel %q: %w", sc.Name, err))
		}
		managedSC = append(managedSC, sc.Name)
	}

	// 5. ConfigurationChannels.
	managedCC := make([]string, 0, len(br.Spec.ConfigChannels))
	for _, cc := range br.Spec.ConfigChannels {
		if err := r.reconcileConfigChannel(ctx, &br, cc); err != nil {
			return r.fail(ctx, &br, "ConfigChannelFailed", fmt.Errorf("configChannel %q: %w", cc.Name, err))
		}
		managedCC = append(managedCC, cc.Name)
	}

	// 6. SystemGroups.
	managedGroups := make([]string, 0, len(br.Spec.SystemGroups))
	for _, sg := range br.Spec.SystemGroups {
		if err := r.reconcileSystemGroup(ctx, &br, sg, orgRef); err != nil {
			return r.fail(ctx, &br, "SystemGroupFailed", fmt.Errorf("group %q: %w", sg.Name, err))
		}
		managedGroups = append(managedGroups, sg.Name)
	}

	// 7. ActivationKeys.
	managedKeys := make([]string, 0, len(br.Spec.ActivationKeys))
	for _, ak := range br.Spec.ActivationKeys {
		if err := r.reconcileActivationKey(ctx, &br, ak, orgRef); err != nil {
			return r.fail(ctx, &br, "ActivationKeyFailed", fmt.Errorf("key %q: %w", ak.Name, err))
		}
		managedKeys = append(managedKeys, ak.Name)
	}

	// 8. ContentProjects + ClmEnvironments.
	managedCP := make([]string, 0, len(br.Spec.ContentProjects))
	for _, cp := range br.Spec.ContentProjects {
		if err := r.reconcileContentProject(ctx, &br, cp, orgRef); err != nil {
			return r.fail(ctx, &br, "ContentProjectFailed", fmt.Errorf("contentProject %q: %w", cp.Name, err))
		}
		managedCP = append(managedCP, brChildName(br.Name, cp.Name))
	}

	// 9. Count systems by type in this namespace.
	var systems uyuniv1.SystemList
	if err := r.List(ctx, &systems, client.InNamespace(br.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	count := uyuniv1.BrandRegionSystemCount{}
	for _, sys := range systems.Items {
		switch sys.Spec.SystemType {
		case uyuniv1.SystemTypeBranchServer:
			count.BranchServer++
		case uyuniv1.SystemTypeStoreHub:
			count.StoreHub++
		case uyuniv1.SystemTypePOS:
			count.POS++
		}
		count.Total++
	}

	br.Status.ManagedProvider = br.Name
	br.Status.ManagedOrganization = br.Name
	br.Status.ManagedRepositories = managedRepos
	br.Status.ManagedSoftwareChannels = managedSC
	br.Status.ManagedConfigChannels = managedCC
	br.Status.ManagedGroups = managedGroups
	br.Status.ManagedActivationKeys = managedKeys
	br.Status.ManagedContentProjects = managedCP
	br.Status.SystemCount = count
	br.Status.ObservedGeneration = br.Generation
	setReady(&br.Status.Conditions, br.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &br); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *BrandRegionReconciler) handleDeletion(ctx context.Context, br *uyuniv1.BrandRegion) (ctrl.Result, error) {
	if !containsFinalizer(br, brFinalizer) {
		return ctrl.Result{}, nil
	}
	// Delete the cluster-scoped UyuniProvider (cannot use owner refs across scopes).
	var prov uyuniv1.UyuniProvider
	if err := r.Get(ctx, types.NamespacedName{Name: br.Name}, &prov); err == nil {
		if delErr := r.Delete(ctx, &prov); delErr != nil && client.IgnoreNotFound(delErr) != nil {
			return ctrl.Result{}, delErr
		}
	}
	removeFinalizer(br, brFinalizer)
	return ctrl.Result{}, r.Update(ctx, br)
}

// ── individual reconcile helpers ─────────────────────────────────────────────

func (r *BrandRegionReconciler) reconcileProvider(ctx context.Context, br *uyuniv1.BrandRegion) error {
	var existing uyuniv1.UyuniProvider
	err := r.Get(ctx, types.NamespacedName{Name: br.Name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		prov := &uyuniv1.UyuniProvider{
			ObjectMeta: metav1.ObjectMeta{Name: br.Name},
			Spec: uyuniv1.UyuniProviderSpec{
				URL:                br.Spec.Provider.URL,
				CredentialsSecretRef: br.Spec.Provider.CredentialsSecretRef,
				InsecureSkipVerify: br.Spec.Provider.InsecureSkipVerify,
				CACertSecretRef:    br.Spec.Provider.CACertSecretRef,
				Timeout:            br.Spec.Provider.Timeout,
			},
		}
		return r.Create(ctx, prov)
	}
	if existing.Spec.URL != br.Spec.Provider.URL {
		existing.Spec.URL = br.Spec.Provider.URL
		return r.Update(ctx, &existing)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileOrganization(ctx context.Context, br *uyuniv1.BrandRegion, providerRef uyuniv1.LocalObjectRef) error {
	var existing uyuniv1.Organization
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: br.Name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		org := &uyuniv1.Organization{
			ObjectMeta: metav1.ObjectMeta{
				Name:            br.Name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: uyuniv1.OrganizationSpec{
				Name:                 br.Spec.Organization.Name,
				ProviderRef:          providerRef,
				CredentialsSecretRef: br.Spec.Organization.CredentialsSecretRef,
				Import:               br.Spec.Organization.Import,
			},
		}
		return r.Create(ctx, org)
	}
	needsUpdate := false
	if existing.Spec.Name != br.Spec.Organization.Name {
		existing.Spec.Name = br.Spec.Organization.Name
		needsUpdate = true
	}
	if !reflect.DeepEqual(existing.Spec.CredentialsSecretRef, br.Spec.Organization.CredentialsSecretRef) {
		existing.Spec.CredentialsSecretRef = br.Spec.Organization.CredentialsSecretRef
		needsUpdate = true
	}
	if !reflect.DeepEqual(existing.Spec.Import, br.Spec.Organization.Import) {
		existing.Spec.Import = br.Spec.Organization.Import
		needsUpdate = true
	}
	if needsUpdate {
		return r.Update(ctx, &existing)
	}
	return nil
}

func brChildName(brName, name string) string {
	return brName + "-" + name
}

func prefixRefs(brName string, refs []uyuniv1.LocalObjectRef) []uyuniv1.LocalObjectRef {
	out := make([]uyuniv1.LocalObjectRef, len(refs))
	for i, r := range refs {
		out[i] = uyuniv1.LocalObjectRef{Name: brChildName(brName, r.Name)}
	}
	return out
}

func prefixRef(brName string, ref *uyuniv1.LocalObjectRef) *uyuniv1.LocalObjectRef {
	if ref == nil {
		return nil
	}
	return &uyuniv1.LocalObjectRef{Name: brChildName(brName, ref.Name)}
}

func (r *BrandRegionReconciler) reconcileRepository(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionRepository, orgRef *uyuniv1.LocalObjectRef) error {
	name := brChildName(br.Name, spec.Name)
	var existing uyuniv1.Repository
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		repoSpec := spec.Spec
		repoSpec.OrganizationRef = orgRef
		repo := &uyuniv1.Repository{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: repoSpec,
		}
		return r.Create(ctx, repo)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileSoftwareChannel(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionSoftwareChannel, orgRef *uyuniv1.LocalObjectRef) error {
	name := brChildName(br.Name, spec.Name)
	var existing uyuniv1.SoftwareChannel
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	expectedRepoRefs := prefixRefs(br.Name, spec.Spec.RepositoryRefs)
	if err != nil {
		scSpec := spec.Spec
		scSpec.OrganizationRef = orgRef
		scSpec.RepositoryRefs = expectedRepoRefs
		scSpec.ParentChannelRef = prefixRef(br.Name, spec.Spec.ParentChannelRef)
		sc := &uyuniv1.SoftwareChannel{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: scSpec,
		}
		return r.Create(ctx, sc)
	}
	needsUpdate := false
	if existing.Spec.OrganizationRef == nil || existing.Spec.OrganizationRef.Name != orgRef.Name {
		existing.Spec.OrganizationRef = orgRef
		needsUpdate = true
	}
	if !reflect.DeepEqual(existing.Spec.RepositoryRefs, expectedRepoRefs) {
		existing.Spec.RepositoryRefs = expectedRepoRefs
		needsUpdate = true
	}
	if needsUpdate {
		return r.Update(ctx, &existing)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileConfigChannel(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionConfigChannel) error {
	name := brChildName(br.Name, spec.Name)
	var existing uyuniv1.ConfigurationChannel
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		cc := &uyuniv1.ConfigurationChannel{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: spec.Spec,
		}
		cc.Spec.Cluster = br.Name
		cc.Spec.OrganizationRef = br.Name
		return r.Create(ctx, cc)
	}
	needsUpdate := false
	if existing.Spec.Cluster != br.Name {
		existing.Spec.Cluster = br.Name
		needsUpdate = true
	}
	if existing.Spec.OrganizationRef != br.Name {
		existing.Spec.OrganizationRef = br.Name
		needsUpdate = true
	}
	if needsUpdate {
		return r.Update(ctx, &existing)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileSystemGroup(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionGroup, orgRef *uyuniv1.LocalObjectRef) error {
	name := brChildName(br.Name, spec.Name)
	var existing uyuniv1.SystemGroup
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	expectedCCRefs := prefixRefs(br.Name, spec.ConfigChannelRefs)
	if err != nil {
		sg := &uyuniv1.SystemGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: uyuniv1.SystemGroupSpec{
				Name:              spec.Name,
				Description:       spec.Description,
				ConfigChannelRefs: expectedCCRefs,
				OrganizationRef:   orgRef,
			},
		}
		return r.Create(ctx, sg)
	}
	wantCCNames := localRefNames(expectedCCRefs)
	haveCCNames := localRefNames(existing.Spec.ConfigChannelRefs)
	if existing.Spec.Description != spec.Description || !stringSlicesEqual(haveCCNames, wantCCNames) {
		existing.Spec.Description = spec.Description
		existing.Spec.ConfigChannelRefs = expectedCCRefs
		return r.Update(ctx, &existing)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileActivationKey(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionActivationKey, orgRef *uyuniv1.LocalObjectRef) error {
	name := brChildName(br.Name, spec.Name)
	var existing uyuniv1.ActivationKey
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		ak := &uyuniv1.ActivationKey{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: uyuniv1.ActivationKeySpec{
				Key:               spec.Name,
				Description:       spec.Description,
				SystemGroupRefs:   prefixRefs(br.Name, spec.SystemGroupRefs),
				Entitlements:      spec.Entitlements,
				BaseChannelRef:    prefixRef(br.Name, br.Spec.BaseChannelRef),
				BaseChannelFrom:   br.Spec.BaseChannelFrom,
				ChildChannelRefs:  prefixRefs(br.Name, br.Spec.ChildChannelRefs),
				ChildChannelsFrom: br.Spec.ChildChannelsFrom,
				OrganizationRef:   orgRef,
			},
		}
		return r.Create(ctx, ak)
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileContentProject(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionContentProject, orgRef *uyuniv1.LocalObjectRef) error {
	cpName := brChildName(br.Name, spec.Name)
	var existing uyuniv1.ContentProject
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: cpName}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		cp := &uyuniv1.ContentProject{
			ObjectMeta: metav1.ObjectMeta{
				Name:            cpName,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: uyuniv1.ContentProjectSpec{
				Label:           spec.Label,
				Name:            spec.DisplayName,
				Description:     spec.Description,
				SourceRefs:      prefixRefs(br.Name, spec.SourceRefs),
				OrganizationRef: orgRef,
			},
		}
		if err := r.Create(ctx, cp); err != nil {
			return err
		}
	}
	for _, env := range spec.Environments {
		if err := r.reconcileClmEnvironment(ctx, br, env, cpName, orgRef); err != nil {
			return fmt.Errorf("environment %q: %w", env.Name, err)
		}
	}
	return nil
}

func (r *BrandRegionReconciler) reconcileClmEnvironment(ctx context.Context, br *uyuniv1.BrandRegion, spec uyuniv1.BrandRegionEnvironment, projectCRName string, orgRef *uyuniv1.LocalObjectRef) error {
	name := brChildName(projectCRName, spec.Name)
	var existing uyuniv1.ClmEnvironment
	err := r.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: name}, &existing)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		clusterRef := &uyuniv1.LocalObjectRef{Name: br.Name}
		env := &uyuniv1.ClmEnvironment{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       br.Namespace,
				OwnerReferences: []metav1.OwnerReference{brandRegionOwnerRef(br)},
			},
			Spec: uyuniv1.ClmEnvironmentSpec{
				Id:              spec.Id,
				Name:            spec.DisplayName,
				Description:     spec.Description,
				ProjectRef:      uyuniv1.LocalObjectRef{Name: projectCRName},
				Predecessor:     spec.Predecessor,
				Cluster:         clusterRef,
				OrganizationRef: orgRef,
			},
		}
		return r.Create(ctx, env)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (r *BrandRegionReconciler) fail(ctx context.Context, br *uyuniv1.BrandRegion, reason string, err error) (ctrl.Result, error) {
	setReady(&br.Status.Conditions, br.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, br)
	return ctrl.Result{}, err
}

func (r *BrandRegionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.BrandRegion{}).
		Owns(&uyuniv1.Organization{}).
		Owns(&uyuniv1.Repository{}).
		Owns(&uyuniv1.SoftwareChannel{}).
		Owns(&uyuniv1.ConfigurationChannel{}).
		Owns(&uyuniv1.SystemGroup{}).
		Owns(&uyuniv1.ActivationKey{}).
		Owns(&uyuniv1.ContentProject{}).
		Owns(&uyuniv1.ClmEnvironment{}).
		Watches(&uyuniv1.System{},
			handler.EnqueueRequestsFromMapFunc(r.brandRegionsForSystem)).
		Complete(r)
}

func (r *BrandRegionReconciler) brandRegionsForSystem(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.BrandRegionList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, len(list.Items))
	for i, br := range list.Items {
		out[i] = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: br.Namespace, Name: br.Name}}
	}
	return out
}

func brandRegionOwnerRef(br *uyuniv1.BrandRegion) metav1.OwnerReference {
	blockOwner := true
	isController := true
	return metav1.OwnerReference{
		APIVersion:         uyuniv1.GroupVersion.String(),
		Kind:               "BrandRegion",
		Name:               br.Name,
		UID:                br.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &blockOwner,
	}
}

func localRefNames(refs []uyuniv1.LocalObjectRef) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return names
}
