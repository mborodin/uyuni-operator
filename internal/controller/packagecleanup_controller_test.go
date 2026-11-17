package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// policyStore is a minimal client.Client holding one PackageCleanupPolicy:
// enough for this reconciler, which only Gets the policy and updates status.
type policyStore struct {
	client.Client
	pol *uyuniv1.PackageCleanupPolicy
}

func (s *policyStore) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.pol.DeepCopyInto(obj.(*uyuniv1.PackageCleanupPolicy))
	return nil
}

func (s *policyStore) Status() client.SubResourceWriter { return policyStatusWriter{s: s} }

type policyStatusWriter struct {
	client.SubResourceWriter
	s *policyStore
}

func (w policyStatusWriter) Update(_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption) error {
	w.s.pol.Status = *obj.(*uyuniv1.PackageCleanupPolicy).Status.DeepCopy()
	return nil
}

type cleanupFakeAPI struct {
	uyuni.API
	pkgs      []uyuni.OrphanedPackage
	installed map[int]bool
	removed   map[int]bool
	checks    int
}

func (f *cleanupFakeAPI) ListPackagesWithoutChannel(context.Context) ([]uyuni.OrphanedPackage, error) {
	var out []uyuni.OrphanedPackage
	for _, p := range f.pkgs {
		if !f.removed[p.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *cleanupFakeAPI) ListProvidingErrata(context.Context, int) ([]uyuni.ErrataRef, error) {
	f.checks++
	return nil, nil
}

func (f *cleanupFakeAPI) ListSystemsWithPackage(_ context.Context, pid int) ([]uyuni.SystemRef, error) {
	if f.installed[pid] {
		return []uyuni.SystemRef{{ID: 1000, Name: "minion"}}, nil
	}
	return nil, nil
}

func (f *cleanupFakeAPI) RemovePackage(_ context.Context, pid int) error {
	f.removed[pid] = true
	return nil
}

type cleanupFakePool struct {
	uyuni.ClientPool
	api uyuni.API
}

func (p cleanupFakePool) ForOrganization(context.Context, string, string) (uyuni.API, error) {
	return p.api, nil
}

// Installed packages lead the oldest-first list; they must not fill every
// batch and block the deletable packages behind them.
func TestPackageCleanupSkippedPackagesDoNotStallBatches(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	api := &cleanupFakeAPI{installed: map[int]bool{}, removed: map[int]bool{}}
	for id := 1; id <= 40; id++ {
		api.pkgs = append(api.pkgs, uyuni.OrphanedPackage{
			ID: id, Name: "pkg", LastModified: now.AddDate(0, 0, -100+id),
		})
		if id <= 25 {
			api.installed[id] = true
		}
	}

	pol := &uyuniv1.PackageCleanupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "brand"},
		Spec: uyuniv1.PackageCleanupPolicySpec{
			OrganizationRef:      &uyuniv1.LocalObjectRef{Name: "org"},
			MinOrphanAgeDays:     7,
			MaxDeletePerRun:      5,
			BatchIntervalSeconds: 60,
		},
	}
	k8s := &policyStore{pol: pol}
	clock := now
	r := &PackageCleanupPolicyReconciler{
		Client:  k8s,
		Clients: cleanupFakePool{api: api},
		Now:     func() time.Time { return clock },
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "brand", Name: "cleanup"}}
	ctx := context.Background()

	var res ctrl.Result
	for run := 0; run < 10 && len(api.removed) < 15; run++ {
		var err error
		res, err = r.Reconcile(ctx, req)
		require.NoError(t, err)
		require.LessOrEqual(t, api.checks, 20*(run+1), "a run must check at most 4x maxDeletePerRun packages")
	}

	require.Len(t, api.removed, 15, "every deletable package is reached despite 25 installed ones ahead of it")
	for id := 1; id <= 25; id++ {
		require.False(t, api.removed[id], "installed package %d must never be deleted", id)
	}

	require.Equal(t, 0, pol.Status.RemainingCandidates)
	require.Equal(t, 25, pol.Status.SkippedInstalled)
	require.Equal(t, idlePollInterval, res.RequeueAfter, "only skipped packages left: go idle instead of looping")

	// After the recheck interval the skipped packages are checked again.
	clock = now.Add(skipRecheckInterval + time.Minute)
	before := api.checks
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.Greater(t, api.checks, before)
	require.Len(t, api.removed, 15)
}
