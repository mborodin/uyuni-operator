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
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=uyuniproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

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

	// Don't delete imported orgs — they pre-existed and may be shared.
	if org.Spec.Import == nil && org.Status.UyuniOrgID > 0 {
		uc, err := r.Clients.For(ctx, toProviderRef(&org.Spec.ProviderRef), org.Namespace)
		if err == nil {
			if delErr := uc.DeleteOrganization(ctx, org.Status.UyuniOrgID); delErr != nil && !uyuni.IsNotFound(delErr) {
				return ctrl.Result{}, delErr
			}
		}
	}

	r.Clients.InvalidateOrg(org.Namespace + "/" + org.Name)
	removeFinalizer(org, orgFinalizer)
	return ctrl.Result{}, r.Update(ctx, org)
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
