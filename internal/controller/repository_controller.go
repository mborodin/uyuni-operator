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

type RepositoryReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var repo uyuniv1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(repo.Spec.OrganizationRef), repo.Namespace)
	if err != nil {
		return r.fail(ctx, &repo, "OrganizationError", err)
	}

	if !repo.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &repo)
	}
	if ensureFinalizer(&repo, repoFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &repo)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &repo, orgRef(repo.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	sslCa, sslCert, sslKey, err := r.resolveSSL(ctx, &repo)
	if err != nil {
		return r.fail(ctx, &repo, "SSLRefError", err)
	}

	current, err := uc.GetRepo(ctx, repo.Spec.Label)
	if uyuni.IsNotFound(err) {
		created, err := uc.CreateRepo(ctx, uyuni.RepoDetails{
			Label:             repo.Spec.Label,
			URL:               repo.Spec.URL,
			Type:              repo.Spec.Type,
			HasSignedMetadata: repo.Spec.HasSignedMetadata,
		}, sslCa, sslCert, sslKey)
		if err != nil {
			return r.fail(ctx, &repo, "CreateFailed", err)
		}
		repo.Status.UyuniID = created.ID
		repo.Status.Label = created.Label
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		repo.Status.UyuniID = current.ID
		repo.Status.Label = current.Label

		// URL is the only mutable field. Drift in immutable fields (Type,
		// HasSignedMetadata) becomes a UyuniDrift condition rather than a
		// fatal error — webhook prevents customer-side drift; Uyuni-side
		// drift (someone edited via WebUI) needs to be surfaced but doesn't
		// block reconciliation of mutable fields.
		if current.URL != repo.Spec.URL {
			if err := uc.UpdateRepoURL(ctx, repo.Spec.Label, repo.Spec.URL); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Detect immutable-field drift.
		drifted := false
		var driftMsg string
		if current.Type != repo.Spec.Type {
			drifted = true
			driftMsg = fmt.Sprintf("type in Uyuni (%s) differs from spec (%s); recreate to reconcile",
				current.Type, repo.Spec.Type)
		}
		if drifted {
			setDrift(&repo.Status.Conditions, repo.Generation, true, "ImmutableFieldDrift", driftMsg)
		} else {
			setDrift(&repo.Status.Conditions, repo.Generation, false, "InSync", "")
		}
	}

	repo.Status.ObservedGeneration = repo.Generation
	setReady(&repo.Status.Conditions, repo.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &repo); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *RepositoryReconciler) handleDeletion(ctx context.Context, uc uyuni.API, repo *uyuniv1.Repository) (ctrl.Result, error) {
	if !containsFinalizer(repo, repoFinalizer) {
		return ctrl.Result{}, nil
	}
	if repo.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(repo, repoFinalizer)
		return ctrl.Result{}, r.Update(ctx, repo)
	}

	// Refuse delete while a SoftwareChannel still references this repo.
	// Without this guard, associateRepo entries would dangle in Uyuni.
	if used, by, err := r.isInUse(ctx, repo); err != nil {
		return ctrl.Result{}, err
	} else if used {
		setReady(&repo.Status.Conditions, repo.Generation, metav1.ConditionFalse,
			"InUse", fmt.Sprintf("SoftwareChannel %q still references this repository", by))
		_ = r.Status().Update(ctx, repo)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if repo.Status.Label != "" {
		if err := uc.DeleteRepo(ctx, repo.Status.Label); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(repo, repoFinalizer)
	return ctrl.Result{}, r.Update(ctx, repo)
}

func (r *RepositoryReconciler) isInUse(ctx context.Context, repo *uyuniv1.Repository) (bool, string, error) {
	var channels uyuniv1.SoftwareChannelList
	if err := r.List(ctx, &channels, client.InNamespace(repo.Namespace)); err != nil {
		return false, "", err
	}
	for _, ch := range channels.Items {
		if !ch.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ref := range ch.Spec.RepositoryRefs {
			if ref.Name == repo.Name {
				return true, ch.Name, nil
			}
		}
	}
	return false, "", nil
}

func (r *RepositoryReconciler) resolveSSL(ctx context.Context, repo *uyuniv1.Repository) (ca, cert, key string, err error) {
	if repo.Spec.SSLCertRef == nil {
		return "", "", "", nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: repo.Namespace, Name: repo.Spec.SSLCertRef.Name,
	}, &sec); err != nil {
		return "", "", "", err
	}
	return string(sec.Data["ca.crt"]), string(sec.Data["tls.crt"]), string(sec.Data["tls.key"]), nil
}

func (r *RepositoryReconciler) fail(ctx context.Context, repo *uyuniv1.Repository, reason string, err error) (ctrl.Result, error) {
	setReady(&repo.Status.Conditions, repo.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, repo)
	return ctrl.Result{}, err
}

func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.Repository{}).
		Complete(r)
}
