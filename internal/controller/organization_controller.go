package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type OrganizationReconciler struct {
	client.Client
	Clients    uyuni.ClientPool
	OperatorNS string
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=uyuniproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=activationkeys;systemgroups;repositories;softwarechannels;configurationchannels;contentprojects;contentprojectpromotions;clmenvironments;systems;tasks;autoinstalldistributions;autoinstallprofiles;imageprofiles,verbs=get;list;watch

func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var org uyuniv1.Organization
	if err := r.Get(ctx, req.NamespacedName, &org); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !org.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &org)
	}
	if ensureFinalizer(&org, orgFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &org)
	}

	// Org lifecycle operations require satellite admin (the provider).
	uc, err := r.Clients.For(ctx, toProviderRef(&org.Spec.ProviderRef), org.Namespace)
	if err != nil {
		return r.fail(ctx, &org, "ProviderError", err)
	}

	// Snapshot the provider's connection details onto our own status. Deletion
	// uses this snapshot instead of re-reading the UyuniProvider CR, since
	// Crossplane compositions may delete sibling managed resources (including
	// the UyuniProvider) concurrently with the Organization, before the
	// Organization's own Uyuni-side cleanup has run.
	if err := r.snapshotProvider(ctx, &org); err != nil {
		return r.fail(ctx, &org, "ProviderError", err)
	}

	if org.Spec.Import != nil {
		details, err := uc.GetOrganizationByID(ctx, org.Spec.Import.OrganizationID)
		if err != nil {
			return r.fail(ctx, &org, "LookupFailed", err)
		}
		org.Status.UyuniOrgID = details.ID
	} else {
		details, err := uc.GetOrganizationByName(ctx, org.Spec.Name)
		if uyuni.IsNotFound(err) {
			orgID, err := r.createOrg(ctx, uc, &org)
			if err != nil {
				return r.fail(ctx, &org, "CreateFailed", err)
			}
			org.Status.UyuniOrgID = orgID
		} else if err != nil {
			return ctrl.Result{}, err
		} else {
			org.Status.UyuniOrgID = details.ID
		}
	}

	// Refresh pool so resources can connect as the org admin immediately.
	r.Clients.InvalidateOrg(org.Namespace + "/" + org.Name)

	org.Status.ObservedGeneration = org.Generation
	setReady(&org.Status.Conditions, org.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// snapshotProvider records the UyuniProvider's connection details onto
// org.Status so deletion doesn't depend on the UyuniProvider CR still
// existing (see clientFromSnapshot).
func (r *OrganizationReconciler) snapshotProvider(ctx context.Context, org *uyuniv1.Organization) error {
	var prov uyuniv1.UyuniProvider
	if err := r.Get(ctx, types.NamespacedName{Name: org.Spec.ProviderRef.Name}, &prov); err != nil {
		return fmt.Errorf("getting UyuniProvider %q: %w", org.Spec.ProviderRef.Name, err)
	}

	credRef := prov.Spec.CredentialsSecretRef
	if credRef.Namespace == "" {
		credRef.Namespace = r.OperatorNS
	}
	org.Status.ProviderURL = prov.Spec.URL
	org.Status.ProviderInsecureSkipVerify = prov.Spec.InsecureSkipVerify
	org.Status.ProviderCredentialsSecretRef = &credRef

	org.Status.ProviderCACertSecretRef = nil
	if prov.Spec.CACertSecretRef != nil {
		caRef := *prov.Spec.CACertSecretRef
		if caRef.Namespace == "" {
			caRef.Namespace = r.OperatorNS
		}
		org.Status.ProviderCACertSecretRef = &caRef
	}
	return nil
}

// clientFromSnapshot builds a Uyuni API client directly from org.Status's
// provider snapshot, bypassing the UyuniProvider CR. Falls back to a live
// lookup if the org was never successfully reconciled (no snapshot yet).
func (r *OrganizationReconciler) clientFromSnapshot(ctx context.Context, org *uyuniv1.Organization) (uyuni.API, error) {
	if org.Status.ProviderURL == "" || org.Status.ProviderCredentialsSecretRef == nil {
		return r.Clients.For(ctx, toProviderRef(&org.Spec.ProviderRef), org.Namespace)
	}

	ref := org.Status.ProviderCredentialsSecretRef
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("reading provider credentials secret: %w", err)
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("provider credentials secret must contain non-empty 'username' and 'password' keys")
	}

	var caCert []byte
	if caRef := org.Status.ProviderCACertSecretRef; caRef != nil {
		var caSecret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: caRef.Namespace, Name: caRef.Name}, &caSecret); err != nil {
			return nil, fmt.Errorf("reading provider CA cert secret: %w", err)
		}
		caCert = caSecret.Data["ca.crt"]
	}

	return uyuni.NewClient(org.Status.ProviderURL, username, password, org.Status.ProviderInsecureSkipVerify, caCert)
}

func (r *OrganizationReconciler) createOrg(ctx context.Context, uc uyuni.API, org *uyuniv1.Organization) (int, error) {
	if org.Spec.CredentialsSecretRef == nil {
		return 0, fmt.Errorf("credentialsSecretRef is required to create a new organization (set spec.import to adopt an existing one)")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: org.Namespace,
		Name:      org.Spec.CredentialsSecretRef.Name,
	}, &secret); err != nil {
		return 0, fmt.Errorf("reading credentials secret: %w", err)
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return 0, fmt.Errorf("credentials secret must contain non-empty 'username' and 'password' keys")
	}
	firstName := string(secret.Data["firstName"])
	if firstName == "" {
		firstName = "Org"
	}
	lastName := string(secret.Data["lastName"])
	if lastName == "" {
		lastName = "Admin"
	}
	email := string(secret.Data["email"])
	if email == "" {
		email = username + "@uyuni.local"
	}
	return uc.CreateOrganization(ctx, org.Spec.Name, username, password, firstName, lastName, email)
}

func (r *OrganizationReconciler) handleDeletion(ctx context.Context, org *uyuniv1.Organization) (ctrl.Result, error) {
	if !containsFinalizer(org, orgFinalizer) {
		return ctrl.Result{}, nil
	}
	if org.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(org, orgFinalizer)
		return ctrl.Result{}, r.Update(ctx, org)
	}

	// Block deletion while org-scoped resources still exist. They need this
	// Organization's credentials (via the client pool) to run their own
	// finalizer-driven Uyuni API cleanup; deleting the Organization first
	// strands them mid-cleanup (cannot authenticate, observed as
	// OrganizationError "not found"). Owner refs alone don't enforce this
	// ordering — blockOwnerDeletion only takes effect under Foreground
	// propagation, and most callers (including Crossplane's provider-
	// kubernetes Delete) use Background. Check explicitly instead.
	if blocker, err := r.organizationDependent(ctx, org); err != nil {
		return ctrl.Result{}, err
	} else if blocker != "" {
		setReady(&org.Status.Conditions, org.Generation, metav1.ConditionFalse, "InUse",
			fmt.Sprintf("waiting for %s to finish its own cleanup before deleting Organization", blocker))
		if err := r.Status().Update(ctx, org); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Don't delete imported orgs — they pre-existed and may be shared.
	if org.Spec.Import == nil && org.Status.UyuniOrgID > 0 {
		uc, err := r.clientFromSnapshot(ctx, org)
		if err != nil {
			return ctrl.Result{}, err
		}
		if delErr := uc.DeleteOrganization(ctx, org.Status.UyuniOrgID); delErr != nil && !uyuni.IsNotFound(delErr) {
			return ctrl.Result{}, delErr
		}
	}

	r.Clients.InvalidateOrg(org.Namespace + "/" + org.Name)
	removeFinalizer(org, orgFinalizer)
	return ctrl.Result{}, r.Update(ctx, org)
}

// organizationDependent returns a human-readable description of the first
// org-scoped resource still referencing org, or "" if none remain.
func (r *OrganizationReconciler) organizationDependent(ctx context.Context, org *uyuniv1.Organization) (string, error) {
	ns := client.InNamespace(org.Namespace)

	var aks uyuniv1.ActivationKeyList
	if err := r.List(ctx, &aks, ns); err != nil {
		return "", err
	}
	for _, x := range aks.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("ActivationKey %q", x.Name), nil
		}
	}

	var sgs uyuniv1.SystemGroupList
	if err := r.List(ctx, &sgs, ns); err != nil {
		return "", err
	}
	for _, x := range sgs.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("SystemGroup %q", x.Name), nil
		}
	}

	var repos uyuniv1.RepositoryList
	if err := r.List(ctx, &repos, ns); err != nil {
		return "", err
	}
	for _, x := range repos.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("Repository %q", x.Name), nil
		}
	}

	var scs uyuniv1.SoftwareChannelList
	if err := r.List(ctx, &scs, ns); err != nil {
		return "", err
	}
	for _, x := range scs.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("SoftwareChannel %q", x.Name), nil
		}
	}

	var ccs uyuniv1.ConfigurationChannelList
	if err := r.List(ctx, &ccs, ns); err != nil {
		return "", err
	}
	for _, x := range ccs.Items {
		if x.Spec.OrganizationRef == org.Name {
			return fmt.Sprintf("ConfigurationChannel %q", x.Name), nil
		}
	}

	var cps uyuniv1.ContentProjectList
	if err := r.List(ctx, &cps, ns); err != nil {
		return "", err
	}
	for _, x := range cps.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("ContentProject %q", x.Name), nil
		}
	}

	var cpps uyuniv1.ContentProjectPromotionList
	if err := r.List(ctx, &cpps, ns); err != nil {
		return "", err
	}
	for _, x := range cpps.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("ContentProjectPromotion %q", x.Name), nil
		}
	}

	var envs uyuniv1.ClmEnvironmentList
	if err := r.List(ctx, &envs, ns); err != nil {
		return "", err
	}
	for _, x := range envs.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("ClmEnvironment %q", x.Name), nil
		}
	}

	var systems uyuniv1.SystemList
	if err := r.List(ctx, &systems, ns); err != nil {
		return "", err
	}
	for _, x := range systems.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("System %q", x.Name), nil
		}
	}

	var tasks uyuniv1.TaskList
	if err := r.List(ctx, &tasks, ns); err != nil {
		return "", err
	}
	for _, x := range tasks.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("Task %q", x.Name), nil
		}
	}

	var ads uyuniv1.AutoinstallDistributionList
	if err := r.List(ctx, &ads, ns); err != nil {
		return "", err
	}
	for _, x := range ads.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("AutoinstallDistribution %q", x.Name), nil
		}
	}

	var aps uyuniv1.AutoinstallProfileList
	if err := r.List(ctx, &aps, ns); err != nil {
		return "", err
	}
	for _, x := range aps.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("AutoinstallProfile %q", x.Name), nil
		}
	}

	var ips uyuniv1.ImageProfileList
	if err := r.List(ctx, &ips, ns); err != nil {
		return "", err
	}
	for _, x := range ips.Items {
		if x.Spec.OrganizationRef != nil && x.Spec.OrganizationRef.Name == org.Name {
			return fmt.Sprintf("ImageProfile %q", x.Name), nil
		}
	}

	return "", nil
}

func (r *OrganizationReconciler) fail(ctx context.Context, org *uyuniv1.Organization, reason string, err error) (ctrl.Result, error) {
	setReady(&org.Status.Conditions, org.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, org)
	return ctrl.Result{}, err
}

func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.Organization{}).
		Complete(r)
}
