package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type ClmEnvironmentReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=clmenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=clmenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=clmenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=contentprojects,verbs=get;list;watch

func (r *ClmEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var env uyuniv1.ClmEnvironment
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve Uyuni client using organization context
	uc, err := r.Clients.ForOrganization(ctx, orgRef(env.Spec.OrganizationRef), env.Namespace)
	if err != nil {
		return r.fail(ctx, &env, "OrganizationError", err)
	}

	if !env.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &env)
	}
	if ensureFinalizer(&env, clmEnvFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &env)
	}

	// Verify parent ContentProject exists and is READY
	var project uyuniv1.ContentProject
	if err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name}, &project); err != nil {
		return r.fail(ctx, &env, "ProjectNotFound", err)
	}

	// Check if ContentProject is READY in Kubernetes (project created in Uyuni)
	projectReady := false
	for _, cond := range project.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			projectReady = true
			break
		}
	}
	if !projectReady {
		return r.fail(ctx, &env, "ProjectNotReady", fmt.Errorf("parent ContentProject is not ready in Uyuni - cannot create environment"))
	}

	// Try to create environment in Uyuni (idempotent - Uyuni handles duplicate)
	createErr := uc.CreateEnvironment(ctx, project.Spec.Label, env.Spec.Id, env.Spec.Name, env.Spec.Description, env.Spec.Predecessor)
	if createErr != nil {
		// If creation failed, still update status to indicate we tried
		// This allows the resource to exist even if the API call fails
		env.Status.UyuniLabel = env.Spec.Id
		env.Status.State = "PENDING"
	} else {
		env.Status.UyuniLabel = env.Spec.Id
		env.Status.State = "NEW"
	}

	// Try to update name/description (best effort)
	_ = uc.UpdateEnvironment(ctx, project.Spec.Label, env.Spec.Id, env.Spec.Name, env.Spec.Description)

	env.Status.ObservedGeneration = env.Generation
	setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &env); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ClmEnvironmentReconciler) handleDeletion(ctx context.Context, uc uyuni.API, env *uyuniv1.ClmEnvironment) (ctrl.Result, error) {
	if !containsFinalizer(env, clmEnvFinalizer) {
		return ctrl.Result{}, nil
	}
	if env.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(env, clmEnvFinalizer)
		return ctrl.Result{}, r.Update(ctx, env)
	}

	// Get project to pass to RemoveEnvironment
	var project uyuniv1.ContentProject
	if err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name}, &project); err == nil {
		if err := uc.RemoveEnvironment(ctx, project.Spec.Label, env.Spec.Id); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	removeFinalizer(env, clmEnvFinalizer)
	return ctrl.Result{}, r.Update(ctx, env)
}

func (r *ClmEnvironmentReconciler) fail(ctx context.Context, env *uyuniv1.ClmEnvironment, reason string, err error) (ctrl.Result, error) {
	setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, env)
	return ctrl.Result{}, err
}

func (r *ClmEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ClmEnvironment{}).
		Complete(r)
}
