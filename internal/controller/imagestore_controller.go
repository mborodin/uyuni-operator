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

type ImageStoreReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=imagestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ImageStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var is uyuniv1.ImageStore
	if err := r.Get(ctx, req.NamespacedName, &is); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !is.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &is)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(is.Spec.OrganizationRef), is.Namespace)
	if err != nil {
		return r.fail(ctx, &is, "OrganizationError", err)
	}

	if ensureFinalizer(&is, isFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &is)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &is, orgRef(is.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	user, pass, err := r.resolveCredentials(ctx, &is)
	if err != nil {
		return r.fail(ctx, &is, "ResolveCredentials", err)
	}

	// Find-or-create in Uyuni. image.store.getDetails returns no numeric id, so
	// realization is tracked via the Ready condition (see resolveStoreLabel).
	current, err := uc.GetImageStore(ctx, is.Spec.Label)
	if uyuni.IsNotFound(err) {
		if createErr := uc.CreateImageStore(ctx, is.Spec.Label, is.Spec.Type, is.Spec.URI, user, pass); createErr != nil {
			return r.fail(ctx, &is, "CreateFailed", createErr)
		}
	} else if err != nil {
		return ctrl.Result{}, err
	} else if current.URI != is.Spec.URI || is.Spec.CredentialsSecretRef != nil {
		// URI drift, or credentials that must be (re-)pushed (getDetails never
		// returns the password, so we can't diff it — re-apply when a secret is set).
		if updateErr := uc.UpdateImageStore(ctx, is.Spec.Label, is.Spec.URI, user, pass); updateErr != nil {
			return r.fail(ctx, &is, "UpdateFailed", updateErr)
		}
	}

	is.Status.ObservedGeneration = is.Generation
	setReady(&is.Status.Conditions, is.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &is); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ImageStoreReconciler) handleDeletion(ctx context.Context, is *uyuniv1.ImageStore) (ctrl.Result, error) {
	if !containsFinalizer(is, isFinalizer) {
		return ctrl.Result{}, nil
	}
	if is.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(is, isFinalizer)
		return ctrl.Result{}, r.Update(ctx, is)
	}
	uc, err := r.Clients.ForOrganization(ctx, orgRef(is.Spec.OrganizationRef), is.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := uc.DeleteImageStore(ctx, is.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(is, isFinalizer)
	return ctrl.Result{}, r.Update(ctx, is)
}

// resolveCredentials reads the username/password from the referenced Secret
// (standard "username"/"password" keys), or returns empty strings when no
// credentials secret is set (e.g. a public registry or the OS image store).
func (r *ImageStoreReconciler) resolveCredentials(ctx context.Context, is *uyuniv1.ImageStore) (user, pass string, err error) {
	if is.Spec.CredentialsSecretRef == nil {
		return "", "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: is.Namespace, Name: is.Spec.CredentialsSecretRef.Name}, &secret); err != nil {
		return "", "", fmt.Errorf("reading credentials secret %q: %w", is.Spec.CredentialsSecretRef.Name, err)
	}
	return string(secret.Data["username"]), string(secret.Data["password"]), nil
}

func (r *ImageStoreReconciler) fail(ctx context.Context, is *uyuniv1.ImageStore, reason string, err error) (ctrl.Result, error) {
	setReady(&is.Status.Conditions, is.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, is)
	return ctrl.Result{}, err
}

func (r *ImageStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ImageStore{}).
		Complete(r)
}
