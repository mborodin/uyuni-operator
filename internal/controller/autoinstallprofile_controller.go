package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type AutoinstallProfileReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstallprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstallprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstallprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=autoinstalldistributions;softwarechannels,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *AutoinstallProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ap uyuniv1.AutoinstallProfile
	if err := r.Get(ctx, req.NamespacedName, &ap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(ap.Spec.OrganizationRef), ap.Namespace)
	if err != nil {
		return r.fail(ctx, &ap, "OrganizationError", err)
	}

	if !ap.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &ap)
	}
	if ensureFinalizer(&ap, apFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &ap)
	}

	// Resolve distribution label from spec.distributionRef.
	distLabel, waitReason, err := r.resolveDistributionLabel(ctx, &ap)
	if err != nil {
		return r.fail(ctx, &ap, "ResolveDistributionFailed", err)
	}
	if waitReason != "" {
		setReady(&ap.Status.Conditions, ap.Generation, metav1.ConditionFalse, "WaitingForDistribution", waitReason)
		_ = r.Status().Update(ctx, &ap)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve child channel labels.
	childLabels, waitReason, err := r.resolveChildChannels(ctx, &ap)
	if err != nil {
		return r.fail(ctx, &ap, "ResolveChildChannelsFailed", err)
	}
	if waitReason != "" {
		setReady(&ap.Status.Conditions, ap.Generation, metav1.ConditionFalse, "WaitingForChildChannel", waitReason)
		_ = r.Status().Update(ctx, &ap)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Read root password from Secret.
	rootPass, err := r.readSecret(ctx, ap.Namespace, ap.Spec.RootPasswordSecretRef.Name, ap.Spec.RootPasswordSecretRef.Key)
	if err != nil {
		return r.fail(ctx, &ap, "SecretReadFailed", err)
	}

	// Ensure the profile exists in Uyuni, creating if necessary.
	_, getErr := uc.GetProfile(ctx, ap.Spec.Label)
	if uyuni.IsNotFound(getErr) {
		if ap.Spec.KickstartContents != "" {
			if importErr := uc.ImportProfile(ctx, uyuni.ProfileImportArgs{
				Label:         ap.Spec.Label,
				TreeLabel:     distLabel,
				KickstartHost: ap.Spec.KickstartHost,
				Contents:      ap.Spec.KickstartContents,
			}); importErr != nil {
				return r.fail(ctx, &ap, "ImportFailed", importErr)
			}
			ap.Status.ContentsHash = hashContents(ap.Spec.KickstartContents)
		} else {
			if createErr := uc.CreateProfile(ctx, uyuni.ProfileCreateArgs{
				Label:              ap.Spec.Label,
				VirtualizationType: ap.Spec.VirtualizationType,
				TreeLabel:          distLabel,
				KickstartHost:      ap.Spec.KickstartHost,
				RootPassword:       rootPass,
				UpdateType:         ap.Spec.UpdateType,
			}); createErr != nil {
				return r.fail(ctx, &ap, "CreateFailed", createErr)
			}
		}
	} else if getErr != nil {
		return ctrl.Result{}, getErr
	} else if ap.Spec.KickstartContents != "" {
		// Profile exists; re-import if the file has changed.
		newHash := hashContents(ap.Spec.KickstartContents)
		if newHash != ap.Status.ContentsHash {
			if importErr := uc.ImportProfile(ctx, uyuni.ProfileImportArgs{
				Label:         ap.Spec.Label,
				TreeLabel:     distLabel,
				KickstartHost: ap.Spec.KickstartHost,
				Contents:      ap.Spec.KickstartContents,
			}); importErr != nil {
				return r.fail(ctx, &ap, "ReImportFailed", importErr)
			}
			ap.Status.ContentsHash = newHash
		}
	}

	// Reconcile mutable profile options (no-op when kickstartContents set,
	// since the imported file is authoritative).
	if ap.Spec.KickstartContents == "" {
		if err := uc.SetProfileUpdateType(ctx, ap.Spec.Label, ap.Spec.UpdateType); err != nil {
			return r.fail(ctx, &ap, "SetUpdateTypeFailed", err)
		}
		if err := uc.SetProfileCfgPreservation(ctx, ap.Spec.Label, ap.Spec.PreserveKsFile); err != nil {
			return r.fail(ctx, &ap, "SetCfgPreservationFailed", err)
		}
	}

	// Reconcile child channels.
	currentChildren, err := uc.GetProfileChildChannels(ctx, ap.Spec.Label)
	if err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if !reflect.DeepEqual(currentChildren, childLabels) {
		if err := uc.SetProfileChildChannels(ctx, ap.Spec.Label, childLabels); err != nil {
			return r.fail(ctx, &ap, "SetChildChannelsFailed", err)
		}
	}

	// Reconcile variables (skip when kickstartContents is set).
	if ap.Spec.KickstartContents == "" && len(ap.Spec.Variables) > 0 {
		currentVars, err := uc.GetProfileVariables(ctx, ap.Spec.Label)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !reflect.DeepEqual(currentVars, ap.Spec.Variables) {
			if err := uc.SetProfileVariables(ctx, ap.Spec.Label, ap.Spec.Variables); err != nil {
				return r.fail(ctx, &ap, "SetVariablesFailed", err)
			}
		}
	}

	// Reconcile scripts (skip when kickstartContents is set).
	if ap.Spec.KickstartContents == "" {
		if err := r.reconcileScripts(ctx, uc, &ap); err != nil {
			return r.fail(ctx, &ap, "ReconcileScriptsFailed", err)
		}
	}

	ap.Status.DistributionLabel = distLabel
	ap.Status.ChildChannelLabels = childLabels
	ap.Status.ObservedGeneration = ap.Generation
	setReady(&ap.Status.Conditions, ap.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &ap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *AutoinstallProfileReconciler) reconcileScripts(ctx context.Context, uc uyuni.API, ap *uyuniv1.AutoinstallProfile) error {
	existing, err := uc.ListProfileScripts(ctx, ap.Spec.Label)
	if err != nil {
		return err
	}

	// Build maps: name → script ID (for existing) and name → spec (for desired).
	currentByName := make(map[string]int, len(existing))
	for _, s := range existing {
		if s.Name != "" {
			currentByName[s.Name] = s.ID
		}
	}
	desiredByName := make(map[string]uyuniv1.AutoinstallScriptSpec, len(ap.Spec.Scripts))
	for _, s := range ap.Spec.Scripts {
		desiredByName[s.Name] = s
	}

	// Add scripts that are in spec but not in Uyuni.
	newScriptIDs := make([]uyuniv1.ProfileScriptStatus, 0, len(ap.Spec.Scripts))
	for _, spec := range ap.Spec.Scripts {
		if id, exists := currentByName[spec.Name]; exists {
			newScriptIDs = append(newScriptIDs, uyuniv1.ProfileScriptStatus{Name: spec.Name, UyuniID: id})
			continue
		}
		id, err := uc.AddProfileScript(ctx, ap.Spec.Label, uyuni.ProfileScript{
			Name:        spec.Name,
			Contents:    spec.Contents,
			Interpreter: spec.Interpreter,
			Type:        spec.Type,
			Chroot:      spec.Chroot,
			Template:    spec.Template,
			ErrorOnFail: spec.ErrorOnFail,
		})
		if err != nil {
			return fmt.Errorf("adding script %q: %w", spec.Name, err)
		}
		newScriptIDs = append(newScriptIDs, uyuniv1.ProfileScriptStatus{Name: spec.Name, UyuniID: id})
	}

	// Remove scripts that are in Uyuni but no longer in spec.
	for name, id := range currentByName {
		if _, stillWanted := desiredByName[name]; !stillWanted {
			if err := uc.RemoveProfileScript(ctx, ap.Spec.Label, id); err != nil {
				return fmt.Errorf("removing script %q (id %d): %w", name, id, err)
			}
		}
	}

	ap.Status.ScriptIDs = newScriptIDs
	return nil
}

func (r *AutoinstallProfileReconciler) handleDeletion(ctx context.Context, uc uyuni.API, ap *uyuniv1.AutoinstallProfile) (ctrl.Result, error) {
	if !containsFinalizer(ap, apFinalizer) {
		return ctrl.Result{}, nil
	}
	if ap.Status.DistributionLabel != "" || ap.Status.ContentsHash != "" {
		if err := uc.DeleteProfile(ctx, ap.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(ap, apFinalizer)
	return ctrl.Result{}, r.Update(ctx, ap)
}

func (r *AutoinstallProfileReconciler) resolveDistributionLabel(ctx context.Context, ap *uyuniv1.AutoinstallProfile) (label, wait string, err error) {
	var ad uyuniv1.AutoinstallDistribution
	if err := r.Get(ctx, types.NamespacedName{Namespace: ap.Namespace, Name: ap.Spec.DistributionRef.Name}, &ad); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", fmt.Sprintf("AutoinstallDistribution %q not found", ap.Spec.DistributionRef.Name), nil
		}
		return "", "", err
	}
	if ad.Status.UyuniID == 0 {
		return "", fmt.Sprintf("AutoinstallDistribution %q not yet realized in Uyuni", ap.Spec.DistributionRef.Name), nil
	}
	return ad.Spec.Label, "", nil
}

func (r *AutoinstallProfileReconciler) resolveChildChannels(ctx context.Context, ap *uyuniv1.AutoinstallProfile) (labels []string, wait string, err error) {
	for _, ref := range ap.Spec.ChildChannelRefs {
		var sc uyuniv1.SoftwareChannel
		if err := r.Get(ctx, types.NamespacedName{Namespace: ap.Namespace, Name: ref.Name}, &sc); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("SoftwareChannel %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if sc.Status.Label == "" {
			return nil, fmt.Sprintf("SoftwareChannel %q not yet realized in Uyuni", ref.Name), nil
		}
		labels = append(labels, sc.Status.Label)
	}
	return labels, "", nil
}

func (r *AutoinstallProfileReconciler) readSecret(ctx context.Context, namespace, name, key string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return "", fmt.Errorf("reading secret %q: %w", name, err)
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", name, key)
	}
	return string(val), nil
}

func (r *AutoinstallProfileReconciler) fail(ctx context.Context, ap *uyuniv1.AutoinstallProfile, reason string, err error) (ctrl.Result, error) {
	setReady(&ap.Status.Conditions, ap.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, ap)
	return ctrl.Result{}, err
}

func hashContents(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func (r *AutoinstallProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.AutoinstallProfile{}).
		Watches(&uyuniv1.AutoinstallDistribution{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForDistribution)).
		Watches(&uyuniv1.SoftwareChannel{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForChannel)).
		Complete(r)
}

func (r *AutoinstallProfileReconciler) profilesForDistribution(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.AutoinstallProfileList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ap := range list.Items {
		if ap.Spec.DistributionRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ap.Namespace, Name: ap.Name},
			})
		}
	}
	return out
}

func (r *AutoinstallProfileReconciler) profilesForChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.AutoinstallProfileList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ap := range list.Items {
		for _, ref := range ap.Spec.ChildChannelRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ap.Namespace, Name: ap.Name},
				})
				break
			}
		}
	}
	return out
}
