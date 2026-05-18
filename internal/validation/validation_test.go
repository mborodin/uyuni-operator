package validation_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/validation"
)

func TestEnvChain(t *testing.T) {
	cases := []struct {
		name     string
		envs     []uyuniv1.ProjectEnvironment
		wantErrs int
		wantPath string
	}{
		{
			name:     "empty",
			envs:     nil,
			wantErrs: 1,
		},
		{
			name: "valid linear chain",
			envs: []uyuniv1.ProjectEnvironment{
				{Label: "dev"},
				{Label: "test", Predecessor: "dev"},
				{Label: "prod", Predecessor: "test"},
			},
		},
		{
			name: "single environment is valid",
			envs: []uyuniv1.ProjectEnvironment{{Label: "only"}},
		},
		{
			name: "two roots",
			envs: []uyuniv1.ProjectEnvironment{
				{Label: "dev"},
				{Label: "alt"},
				{Label: "prod", Predecessor: "dev"},
			},
			wantErrs: 1,
		},
		{
			name: "duplicate labels",
			envs: []uyuniv1.ProjectEnvironment{
				{Label: "dev"},
				{Label: "dev", Predecessor: "dev"},
			},
			wantErrs: 1,
			wantPath: "spec.environments[1].label",
		},
		{
			name: "predecessor refers to unknown",
			envs: []uyuniv1.ProjectEnvironment{
				{Label: "dev"},
				{Label: "prod", Predecessor: "test"},
			},
			wantErrs: 1,
			wantPath: "spec.environments[1].predecessor",
		},
		{
			name: "cycle a->b->a",
			envs: []uyuniv1.ProjectEnvironment{
				{Label: "a", Predecessor: "b"},
				{Label: "b", Predecessor: "a"},
			},
			// No root + cycle detection gated on no other errors → only
			// the "no root" error reports. That's acceptable: customer
			// fixes structure first, cycle becomes apparent next round.
			wantErrs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validation.EnvChain(tc.envs, field.NewPath("spec.environments"))
			require.Len(t, errs, tc.wantErrs, "errors: %v", errs)
			if tc.wantPath != "" {
				found := false
				for _, e := range errs {
					if e.Field == tc.wantPath {
						found = true
						break
					}
				}
				require.True(t, found,
					"expected error at field %q, got %v", tc.wantPath, errs)
			}
		})
	}
}

func TestChainOrder(t *testing.T) {
	envs := []uyuniv1.ProjectEnvironment{
		{Label: "prod", Predecessor: "test"},
		{Label: "dev"},
		{Label: "test", Predecessor: "dev"},
	}
	ordered := validation.ChainOrder(envs)
	require.Len(t, ordered, 3)
	require.Equal(t, "dev", ordered[0].Label)
	require.Equal(t, "test", ordered[1].Label)
	require.Equal(t, "prod", ordered[2].Label)
}

func TestPromotionPair(t *testing.T) {
	cp := &uyuniv1.ContentProject{
		Spec: uyuniv1.ContentProjectSpec{
			Environments: []uyuniv1.ProjectEnvironment{
				{Label: "dev"},
				{Label: "test", Predecessor: "dev"},
				{Label: "prod", Predecessor: "test"},
			},
		},
	}
	fromPath := field.NewPath("spec.fromEnvironment")
	toPath := field.NewPath("spec.toEnvironment")

	t.Run("valid adjacent promotion", func(t *testing.T) {
		errs := validation.PromotionPair(cp, "dev", "test", fromPath, toPath)
		require.Empty(t, errs)
	})

	t.Run("non-adjacent rejected", func(t *testing.T) {
		errs := validation.PromotionPair(cp, "dev", "prod", fromPath, toPath)
		require.Len(t, errs, 1)
		require.Equal(t, "spec.toEnvironment", errs[0].Field)
	})

	t.Run("unknown source", func(t *testing.T) {
		errs := validation.PromotionPair(cp, "staging", "prod", fromPath, toPath)
		// fromEnv unknown + chain-adjacency check skipped because fromEnv missing.
		require.Len(t, errs, 1)
		require.Equal(t, "spec.fromEnvironment", errs[0].Field)
	})

	t.Run("same env", func(t *testing.T) {
		errs := validation.PromotionPair(cp, "dev", "dev", fromPath, toPath)
		require.Len(t, errs, 1)
		require.Contains(t, errs[0].Detail, "differ")
	})
}

func TestTaskSpec(t *testing.T) {
	t.Run("zero kinds rejected", func(t *testing.T) {
		s := &uyuniv1.TaskSpec{
			Target: uyuniv1.SystemTarget{
				SystemRef: &uyuniv1.LocalObjectRef{Name: "web-01"},
			},
		}
		errs := validation.TaskSpec(s, field.NewPath("spec"))
		require.Len(t, errs, 1)
	})

	t.Run("two kinds rejected", func(t *testing.T) {
		s := &uyuniv1.TaskSpec{
			Target: uyuniv1.SystemTarget{
				SystemRef: &uyuniv1.LocalObjectRef{Name: "web-01"},
			},
			Highstate: &uyuniv1.HighstateSpec{},
			Reboot:    &uyuniv1.RebootSpec{},
		}
		errs := validation.TaskSpec(s, field.NewPath("spec"))
		require.Len(t, errs, 1)
	})

	t.Run("remoteCommand requires command", func(t *testing.T) {
		s := &uyuniv1.TaskSpec{
			Target: uyuniv1.SystemTarget{
				SystemRef: &uyuniv1.LocalObjectRef{Name: "web-01"},
			},
			RemoteCommand: &uyuniv1.RemoteCommandSpec{},
		}
		errs := validation.TaskSpec(s, field.NewPath("spec"))
		require.Len(t, errs, 1)
		require.Equal(t, "spec.remoteCommand.command", errs[0].Field)
	})

	t.Run("two target styles rejected", func(t *testing.T) {
		s := &uyuniv1.TaskSpec{
			Target: uyuniv1.SystemTarget{
				SystemRef:      &uyuniv1.LocalObjectRef{Name: "web-01"},
				SystemGroupRef: &uyuniv1.LocalObjectRef{Name: "linux-prod"},
			},
			Highstate: &uyuniv1.HighstateSpec{},
		}
		errs := validation.TaskSpec(s, field.NewPath("spec"))
		require.Len(t, errs, 1)
		require.Equal(t, "spec.target", errs[0].Field)
	})

	t.Run("valid spec passes", func(t *testing.T) {
		s := &uyuniv1.TaskSpec{
			Target: uyuniv1.SystemTarget{
				SystemRef: &uyuniv1.LocalObjectRef{Name: "web-01"},
			},
			Highstate: &uyuniv1.HighstateSpec{Test: true},
		}
		require.Empty(t, validation.TaskSpec(s, field.NewPath("spec")))
	})
}

func TestChannelRefMutex(t *testing.T) {
	t.Run("both base styles rejected", func(t *testing.T) {
		errs := validation.ChannelRefMutex(
			&uyuniv1.LocalObjectRef{Name: "ch"},
			&uyuniv1.ChannelFromProject{
				ContentProjectRef:  uyuniv1.LocalObjectRef{Name: "p"},
				Environment:        "dev",
				SourceChannelLabel: "x",
			},
			nil, nil, field.NewPath("spec"))
		require.Len(t, errs, 1)
	})

	t.Run("both child styles rejected", func(t *testing.T) {
		errs := validation.ChannelRefMutex(
			nil, nil,
			[]uyuniv1.LocalObjectRef{{Name: "a"}},
			[]uyuniv1.ChannelFromProject{{
				ContentProjectRef:  uyuniv1.LocalObjectRef{Name: "p"},
				Environment:        "dev",
				SourceChannelLabel: "x",
			}},
			field.NewPath("spec"))
		require.Len(t, errs, 1)
	})

	t.Run("only base styles set, both child empty - ok", func(t *testing.T) {
		errs := validation.ChannelRefMutex(
			&uyuniv1.LocalObjectRef{Name: "ch"},
			nil, nil, nil,
			field.NewPath("spec"))
		require.Empty(t, errs)
	})

	t.Run("nothing set - ok (allows minimal CRs)", func(t *testing.T) {
		errs := validation.ChannelRefMutex(nil, nil, nil, nil, field.NewPath("spec"))
		require.Empty(t, errs)
	})
}

func TestPreCreateRequiresIdentification(t *testing.T) {
	t.Run("preCreate=false always ok", func(t *testing.T) {
		errs := validation.PreCreateRequiresIdentification(
			false, "", nil, field.NewPath("spec"))
		require.Empty(t, errs)
	})

	t.Run("preCreate with hostname", func(t *testing.T) {
		errs := validation.PreCreateRequiresIdentification(
			true, "web.example.com", nil, field.NewPath("spec"))
		require.Empty(t, errs)
	})

	t.Run("preCreate with MAC", func(t *testing.T) {
		errs := validation.PreCreateRequiresIdentification(
			true, "",
			[]uyuniv1.NetworkInterface{{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:ff"}},
			field.NewPath("spec"))
		require.Empty(t, errs)
	})

	t.Run("preCreate with no identification rejected", func(t *testing.T) {
		errs := validation.PreCreateRequiresIdentification(
			true, "", nil, field.NewPath("spec"))
		require.Len(t, errs, 1)
	})

	t.Run("preCreate with MAC-less interfaces still rejected", func(t *testing.T) {
		errs := validation.PreCreateRequiresIdentification(
			true, "",
			[]uyuniv1.NetworkInterface{{Name: "eth0"}},
			field.NewPath("spec"))
		require.Len(t, errs, 1)
	})
}

func TestStrictBooleanAnnotations(t *testing.T) {
	t.Run("true accepted", func(t *testing.T) {
		errs := validation.StrictBooleanAnnotations(
			map[string]string{uyuniv1.AnnForceDelete: "true"},
			[]string{uyuniv1.AnnForceDelete},
			field.NewPath("metadata.annotations"))
		require.Empty(t, errs)
	})

	t.Run("absent accepted", func(t *testing.T) {
		errs := validation.StrictBooleanAnnotations(
			map[string]string{},
			[]string{uyuniv1.AnnForceDelete},
			field.NewPath("metadata.annotations"))
		require.Empty(t, errs)
	})

	t.Run("yes rejected", func(t *testing.T) {
		errs := validation.StrictBooleanAnnotations(
			map[string]string{uyuniv1.AnnForceDelete: "yes"},
			[]string{uyuniv1.AnnForceDelete},
			field.NewPath("metadata.annotations"))
		require.Len(t, errs, 1)
	})

	t.Run("True (case mismatch) rejected", func(t *testing.T) {
		errs := validation.StrictBooleanAnnotations(
			map[string]string{uyuniv1.AnnForceDelete: "True"},
			[]string{uyuniv1.AnnForceDelete},
			field.NewPath("metadata.annotations"))
		require.Len(t, errs, 1)
	})
}
