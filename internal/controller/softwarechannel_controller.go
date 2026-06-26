package controller

import (
	"context"
	"fmt"
	"strings"
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

type SoftwareChannelReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=softwarechannels/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=repositories,verbs=get;list;watch

func (r *SoftwareChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sc uyuniv1.SoftwareChannel
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &sc)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(sc.Spec.OrganizationRef), sc.Namespace)
	if err != nil {
		return r.fail(ctx, &sc, "OrganizationError", err)
	}

	if ensureFinalizer(&sc, scFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &sc)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &sc, orgRef(sc.Spec.OrganizationRef)); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve parent channel label if a ref is given.
	parentLabel, waitReason, err := r.resolveParentChannel(ctx, &sc)
	if err != nil {
		return r.fail(ctx, &sc, "ResolveParentFailed", err)
	}
	if waitReason != "" {
		setReady(&sc.Status.Conditions, sc.Generation, metav1.ConditionFalse, "WaitingForParentChannel", waitReason)
		_ = r.Status().Update(ctx, &sc)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Create or adopt the channel in Uyuni.
	justCreated := false
	current, err := uc.GetChannel(ctx, sc.Spec.Label)
	if uyuni.IsNotFound(err) {
		arch := sc.Spec.Arch
		if arch == "" {
			arch = "channel-x86_64"
		}
		checksum := sc.Spec.Checksum
		if checksum == "" {
			checksum = "sha256"
		}
		createErr := uc.CreateChannel(ctx, uyuni.ChannelDetails{
			Label:              sc.Spec.Label,
			Name:               sc.Spec.Name,
			Summary:            sc.Spec.Summary,
			Description:        sc.Spec.Description,
			ArchName:           arch,
			ParentChannelLabel: parentLabel,
			ChecksumLabel:      checksum,
			GPGKeyURL:          sc.Spec.GPGKey.URL,
			GPGKeyID:           sc.Spec.GPGKey.KeyID,
			GPGKeyFp:           sc.Spec.GPGKey.Fingerprint,
			GPGCheck:           sc.Spec.GPGKey.Check,
		})
		if createErr != nil {
			if strings.Contains(strings.ToLower(createErr.Error()), "already exists") ||
				strings.Contains(strings.ToLower(createErr.Error()), "duplicate") {
				current, err = uc.GetChannel(ctx, sc.Spec.Label)
				if err != nil {
					return r.fail(ctx, &sc, "CreateFailed", createErr)
				}
			} else {
				return r.fail(ctx, &sc, "CreateFailed", createErr)
			}
		} else {
			justCreated = true
			current, err = uc.GetChannel(ctx, sc.Spec.Label)
			if err != nil {
				return r.fail(ctx, &sc, "GetAfterCreate", err)
			}
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	sc.Status.UyuniID = current.ID
	sc.Status.Label = current.Label

	// Sync mutable fields.
	if channelNeedsUpdate(current, &sc) {
		if err := uc.SetChannelDetails(ctx, current.ID, uyuni.ChannelDetails{
			Name:          sc.Spec.Name,
			Summary:       sc.Spec.Summary,
			Description:   sc.Spec.Description,
			GPGKeyURL:     sc.Spec.GPGKey.URL,
			GPGKeyID:      sc.Spec.GPGKey.KeyID,
			GPGKeyFp:      sc.Spec.GPGKey.Fingerprint,
			GPGCheck:      sc.Spec.GPGKey.Check,
			ChecksumLabel: sc.Spec.Checksum,
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Detect immutable-field drift (arch and parent channel can't be changed
	// in Uyuni after creation; webhook prevents customer-side drift, this
	// surfaces external drift from a WebUI/API edit instead).
	desiredArch := sc.Spec.Arch
	if desiredArch == "" {
		desiredArch = "channel-x86_64"
	}
	// channel.software.getDetails normalizes arch_name by stripping the
	// "channel-" prefix that create's archLabel param requires (e.g.
	// "channel-x86_64" is created, but read back as "x86_64").
	currentArch := strings.TrimPrefix(current.ArchName, "channel-")
	wantArch := strings.TrimPrefix(desiredArch, "channel-")
	drifted := false
	var driftMsg string
	if currentArch != wantArch {
		drifted = true
		driftMsg = fmt.Sprintf("arch in Uyuni (%s) differs from spec (%s); recreate to reconcile",
			current.ArchName, desiredArch)
	} else if current.ParentChannelLabel != parentLabel {
		drifted = true
		driftMsg = fmt.Sprintf("parent channel in Uyuni (%q) differs from spec (%q); recreate to reconcile",
			current.ParentChannelLabel, parentLabel)
	}
	if drifted {
		setDrift(&sc.Status.Conditions, sc.Generation, true, "ImmutableFieldDrift", driftMsg)
	} else {
		setDrift(&sc.Status.Conditions, sc.Generation, false, "InSync", "")
	}

	// Resolve desired repository associations.
	desiredRepos, repoWait, err := r.resolveRepos(ctx, &sc)
	if err != nil {
		return r.fail(ctx, &sc, "ResolveReposFailed", err)
	}
	if repoWait != "" {
		setReady(&sc.Status.Conditions, sc.Generation, metav1.ConditionFalse, "WaitingForRepository", repoWait)
		_ = r.Status().Update(ctx, &sc)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Sync repo associations.
	currentRepos, err := uc.ListChannelRepos(ctx, sc.Spec.Label)
	if err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	addRepos, rmRepos := diffStringSets(currentRepos, desiredRepos)
	for _, label := range addRepos {
		if err := uc.AssociateRepo(ctx, sc.Spec.Label, label); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, label := range rmRepos {
		if err := uc.DisassociateRepo(ctx, sc.Spec.Label, label); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set recurring sync schedule if configured.
	if sc.Spec.Sync.Cron != "" {
		if err := uc.SetRepoSyncSchedule(ctx, sc.Spec.Label, sc.Spec.Sync.Cron); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle one-off sync annotation.
	if sc.Annotations[uyuniv1.AnnSyncNow] == "true" {
		if err := uc.SyncChannelNow(ctx, sc.Spec.Label); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		sc.Status.LastSyncTime = &now
		delete(sc.Annotations, uyuniv1.AnnSyncNow)
		if err := r.Update(ctx, &sc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Trigger sync after first creation if requested.
	if justCreated && sc.Spec.Sync.SyncOnCreate {
		if err := uc.SyncChannelNow(ctx, sc.Spec.Label); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		sc.Status.LastSyncTime = &now
	}

	sc.Status.AssociatedRepos = desiredRepos
	sc.Status.PackageCount = current.PackageCount
	sc.Status.ObservedGeneration = sc.Generation
	setReady(&sc.Status.Conditions, sc.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &sc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *SoftwareChannelReconciler) handleDeletion(ctx context.Context, sc *uyuniv1.SoftwareChannel) (ctrl.Result, error) {
	if !containsFinalizer(sc, scFinalizer) {
		return ctrl.Result{}, nil
	}
	if sc.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(sc, scFinalizer)
		return ctrl.Result{}, r.Update(ctx, sc)
	}
	if sc.Status.UyuniID != 0 {
		uc, err := r.Clients.ForOrganization(ctx, orgRef(sc.Spec.OrganizationRef), sc.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := uc.DeleteChannel(ctx, sc.Spec.Label); err != nil && !uyuni.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	removeFinalizer(sc, scFinalizer)
	return ctrl.Result{}, r.Update(ctx, sc)
}

func (r *SoftwareChannelReconciler) resolveParentChannel(ctx context.Context, sc *uyuniv1.SoftwareChannel) (label, wait string, err error) {
	if sc.Spec.ParentChannelRef == nil {
		return "", "", nil
	}
	var parent uyuniv1.SoftwareChannel
	if err := r.Get(ctx, types.NamespacedName{Namespace: sc.Namespace, Name: sc.Spec.ParentChannelRef.Name}, &parent); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", fmt.Sprintf("parent SoftwareChannel %q not found", sc.Spec.ParentChannelRef.Name), nil
		}
		return "", "", err
	}
	if parent.Status.UyuniID == 0 {
		return "", fmt.Sprintf("parent SoftwareChannel %q not yet realized in Uyuni", sc.Spec.ParentChannelRef.Name), nil
	}
	return parent.Spec.Label, "", nil
}

func (r *SoftwareChannelReconciler) resolveRepos(ctx context.Context, sc *uyuniv1.SoftwareChannel) (labels []string, wait string, err error) {
	for _, ref := range sc.Spec.RepositoryRefs {
		var repo uyuniv1.Repository
		if err := r.Get(ctx, types.NamespacedName{Namespace: sc.Namespace, Name: ref.Name}, &repo); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("Repository %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if repo.Status.Label == "" {
			return nil, fmt.Sprintf("Repository %q not yet realized in Uyuni", ref.Name), nil
		}
		labels = append(labels, repo.Status.Label)
	}
	return labels, "", nil
}

func (r *SoftwareChannelReconciler) fail(ctx context.Context, sc *uyuniv1.SoftwareChannel, reason string, err error) (ctrl.Result, error) {
	setReady(&sc.Status.Conditions, sc.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, sc)
	return ctrl.Result{}, err
}

func channelNeedsUpdate(current *uyuni.ChannelDetails, sc *uyuniv1.SoftwareChannel) bool {
	return current.Name != sc.Spec.Name ||
		current.Summary != sc.Spec.Summary ||
		current.Description != sc.Spec.Description ||
		current.GPGKeyURL != sc.Spec.GPGKey.URL ||
		current.GPGKeyID != sc.Spec.GPGKey.KeyID ||
		current.GPGKeyFp != sc.Spec.GPGKey.Fingerprint ||
		current.GPGCheck != sc.Spec.GPGKey.Check ||
		current.ChecksumLabel != sc.Spec.Checksum
}

func (r *SoftwareChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.SoftwareChannel{}).
		Watches(&uyuniv1.Repository{},
			handler.EnqueueRequestsFromMapFunc(r.channelsForRepository)).
		Complete(r)
}

func (r *SoftwareChannelReconciler) channelsForRepository(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SoftwareChannelList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sc := range list.Items {
		for _, ref := range sc.Spec.RepositoryRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sc.Namespace, Name: sc.Name},
				})
				break
			}
		}
	}
	return out
}
