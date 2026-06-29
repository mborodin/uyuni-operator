package controller

import (
	"context"
	"crypto/md5"
	"fmt"
	"regexp"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/git"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type ConfigurationChannelReconciler struct {
	client.Client
	Clients uyuni.ClientPool
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=configurationchannels/finalizers,verbs=update

func (r *ConfigurationChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cc uyuniv1.ConfigurationChannel
	if err := r.Get(ctx, req.NamespacedName, &cc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &cc)
	}

	uc, err := r.resolveClient(ctx, &cc)
	if err != nil {
		return r.fail(ctx, &cc, "ProviderError", err)
	}

	if ensureFinalizer(&cc, confChanFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cc)
	}

	if err := reconcileOrganizationOwnership(ctx, r.Client, &cc, cc.Spec.OrganizationRef); err != nil {
		return ctrl.Result{}, err
	}

	current, err := uc.GetConfigChannel(ctx, cc.Spec.ID)
	if uyuni.IsNotFound(err) {
		created, createErr := uc.CreateConfigChannel(ctx, cc.Spec.ID, cc.Spec.Name, cc.Spec.Description, cc.Spec.Type)
		if createErr != nil {
			return r.fail(ctx, &cc, "CreateFailed", createErr)
		}
		cc.Status.UyuniID = created.ID
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		cc.Status.UyuniID = current.ID

		if current.Name != cc.Spec.Name || current.Description != cc.Spec.Description {
			if err := uc.UpdateConfigChannel(ctx, cc.Spec.ID, cc.Spec.Name, cc.Spec.Description); err != nil {
				return ctrl.Result{}, err
			}
		}

		if current.Type != cc.Spec.Type {
			setDrift(&cc.Status.Conditions, cc.Generation, true, "ImmutableFieldDrift",
				fmt.Sprintf("type in Uyuni (%s) differs from spec (%s); recreate to reconcile",
					current.Type, cc.Spec.Type))
		} else {
			setDrift(&cc.Status.Conditions, cc.Generation, false, "InSync", "")
		}
	}

	// Sync repository files if configured
	if cc.Spec.AutoSync != nil && *cc.Spec.AutoSync && cc.Spec.URL != "" {
		if r.shouldSync(&cc) {
			files, repoHash, syncErr := r.syncRepositoryFiles(ctx, &cc)
			if syncErr != nil {
				setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionFalse, "RepoSyncFailed", syncErr.Error())
				cc.Status.SyncStatus = "Failed"
				_ = r.Status().Update(ctx, &cc)
				return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
			}

			// Upload files to Uyuni
			if len(files) > 0 {
				uploadErr := r.uploadFilesToUyuni(ctx, uc, &cc, files)
				if uploadErr != nil {
					setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionFalse, "FileUploadFailed", uploadErr.Error())
					cc.Status.SyncStatus = "Failed"
					_ = r.Status().Update(ctx, &cc)
					return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
				}
			}

			// Update sync status
			cc.Status.SyncStatus = "Synced"
			cc.Status.LastSyncTime = &metav1.Time{Time: time.Now()}
			cc.Status.SyncedFileCount = len(files)
			cc.Status.RepositoryHash = repoHash
			setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionTrue, "RepoSynced",
				fmt.Sprintf("Synced %d files from repository", len(files)))

			// Clear sync-now annotation if present
			if cc.Annotations != nil && cc.Annotations["sync-now"] == "true" {
				delete(cc.Annotations, "sync-now")
				_ = r.Update(ctx, &cc)
			}
		}
	} else if cc.Spec.AutoSync != nil && *cc.Spec.AutoSync && cc.Spec.URL == "" {
		cc.Status.SyncStatus = "NotConfigured"
		setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionFalse, "RepoNotConfigured",
			"autoSync enabled but URL is not set")
		_ = r.Status().Update(ctx, &cc)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	cc.Status.ObservedGeneration = cc.Generation
	setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &cc); err != nil {
		return ctrl.Result{}, err
	}

	// Determine requeue time based on sync schedule
	requeueAfter := 5 * time.Minute
	if cc.Spec.AutoSync != nil && *cc.Spec.AutoSync && cc.Spec.SyncSchedule != "" {
		// Use sync schedule interval for requeue
		requeueAfter = parseSyncInterval(cc.Spec.SyncSchedule)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *ConfigurationChannelReconciler) handleDeletion(ctx context.Context, cc *uyuniv1.ConfigurationChannel) (ctrl.Result, error) {
	if !containsFinalizer(cc, confChanFinalizer) {
		return ctrl.Result{}, nil
	}
	if cc.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(cc, confChanFinalizer)
		return ctrl.Result{}, r.Update(ctx, cc)
	}
	uc, err := r.resolveClient(ctx, cc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := uc.DeleteConfigChannel(ctx, cc.Spec.ID); err != nil && !uyuni.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	removeFinalizer(cc, confChanFinalizer)
	return ctrl.Result{}, r.Update(ctx, cc)
}

func (r *ConfigurationChannelReconciler) resolveClient(ctx context.Context, cc *uyuniv1.ConfigurationChannel) (uyuni.API, error) {
	if cc.Spec.OrganizationRef != "" {
		return r.Clients.ForOrganization(ctx, cc.Spec.OrganizationRef, cc.Namespace)
	}
	var clusterRef *uyuni.LocalObjectRef
	if cc.Spec.Cluster != "" {
		clusterRef = &uyuni.LocalObjectRef{Name: cc.Spec.Cluster}
	}
	return r.Clients.For(ctx, clusterRef, cc.Namespace)
}

func (r *ConfigurationChannelReconciler) fail(ctx context.Context, cc *uyuniv1.ConfigurationChannel, reason string, err error) (ctrl.Result, error) {
	setReady(&cc.Status.Conditions, cc.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, cc)
	return ctrl.Result{}, err
}

func (r *ConfigurationChannelReconciler) shouldSync(cc *uyuniv1.ConfigurationChannel) bool {
	if cc.Spec.AutoSync == nil || !*cc.Spec.AutoSync || cc.Spec.URL == "" {
		return false
	}

	// Check for manual sync trigger annotation
	if cc.Annotations != nil && cc.Annotations["sync-now"] == "true" {
		return true
	}

	// If never synced, should sync
	if cc.Status.LastSyncTime == nil {
		return true
	}

	// If no schedule set, don't sync periodically
	if cc.Spec.SyncSchedule == "" {
		return false
	}

	// Parse sync interval from schedule (e.g., "every 2m", "every 1h")
	syncInterval := parseSyncInterval(cc.Spec.SyncSchedule)
	return time.Since(cc.Status.LastSyncTime.Time) >= syncInterval
}

func parseSyncInterval(schedule string) time.Duration {
	// Match "every Nm" or "every Nh" format
	re := regexp.MustCompile(`every\s+(\d+)([mh])`)
	matches := re.FindStringSubmatch(schedule)
	if len(matches) != 3 {
		return 6 * time.Hour // default fallback
	}

	num, _ := strconv.Atoi(matches[1])
	unit := matches[2]

	if unit == "m" {
		return time.Duration(num) * time.Minute
	} else if unit == "h" {
		return time.Duration(num) * time.Hour
	}
	return 6 * time.Hour
}

func (r *ConfigurationChannelReconciler) syncRepositoryFiles(ctx context.Context, cc *uyuniv1.ConfigurationChannel) (map[string]string, string, error) {
	if cc.Spec.URL == "" {
		return nil, "", fmt.Errorf("repository URL is required for sync")
	}

	gitClient := git.New()

	subPath := cc.Spec.RepositoryPath
	if subPath == "" {
		subPath = "."
	}

	ref := cc.Spec.RepositoryRef
	if ref == "" {
		ref = "main" // default to main branch
	}

	files, hash, err := gitClient.Clone(cc.Spec.URL, ref, subPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to sync repository: %w", err)
	}

	return files, hash, nil
}

func (r *ConfigurationChannelReconciler) uploadFilesToUyuni(ctx context.Context, uc uyuni.API, cc *uyuniv1.ConfigurationChannel, files map[string]string) error {
	for filePath, content := range files {
		fileUpsert := uyuni.ConfigFileUpsert{
			Path:        filePath,
			Contents:    content,
			Type:        "file",
			Owner:       "root",
			Group:       "root",
			Permissions: "0644",
		}
		_, err := uc.CreateOrUpdateConfigFile(ctx, cc.Spec.ID, fileUpsert)
		if err != nil {
			return fmt.Errorf("failed to upload file %s: %w", filePath, err)
		}
	}
	return nil
}

func (r *ConfigurationChannelReconciler) calculateFilesHash(files map[string]string) string {
	h := md5.New()
	for _, path := range sortedMapKeys(files) {
		h.Write([]byte(path + ":" + files[path]))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple sort - full implementation would use sort package
	for i := 0; i < len(keys)-1; i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func (r *ConfigurationChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.ConfigurationChannel{}).
		Complete(r)
}
