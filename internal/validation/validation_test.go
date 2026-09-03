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
	// known/pred are what the webhook builds by merging
	// ContentProject.spec.environments (deprecated) with live
	// ClmEnvironment objects - PromotionPair itself takes plain data, no
	// I/O (see promotion.go doc comment).
	known := map[string]bool{"dev": true, "test": true, "prod": true}
	pred := map[string]string{"dev": "", "test": "dev", "prod": "test"}
	fromPath := field.NewPath("spec.fromEnvironment")
	toPath := field.NewPath("spec.toEnvironment")

	t.Run("valid adjacent promotion", func(t *testing.T) {
		errs := validation.PromotionPair(known, pred, "dev", "test", fromPath, toPath)
		require.Empty(t, errs)
	})

	t.Run("non-adjacent rejected", func(t *testing.T) {
		errs := validation.PromotionPair(known, pred, "dev", "prod", fromPath, toPath)
		require.Len(t, errs, 1)
		require.Equal(t, "spec.toEnvironment", errs[0].Field)
	})

	t.Run("unknown source", func(t *testing.T) {
		errs := validation.PromotionPair(known, pred, "staging", "prod", fromPath, toPath)
		// fromEnv unknown + chain-adjacency check skipped because fromEnv missing.
		require.Len(t, errs, 1)
		require.Equal(t, "spec.fromEnvironment", errs[0].Field)
	})

	t.Run("same env", func(t *testing.T) {
		errs := validation.PromotionPair(known, pred, "dev", "dev", fromPath, toPath)
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

func TestSystemFormulas(t *testing.T) {
	cmRef := &uyuniv1.ConfigMapKeyRef{Name: "cfg", Key: "config.yaml"}
	objRef := &uyuniv1.ObjectFieldRef{
		APIVersion: "uyuni.uyuni-project.org/v1alpha1", Kind: "ImageProfile",
		Name: "p", FieldPath: "status.imageUrl",
	}
	cases := []struct {
		name     string
		vf       uyuniv1.FormulaValueFrom
		wantErrs int
		wantPath string
	}{
		{
			name: "yaml configmap with path",
			vf:   uyuniv1.FormulaValueFrom{Path: "branch", Format: "yaml", ConfigMapKeyRef: cmRef},
		},
		{
			name: "yaml configmap empty path merges at root",
			vf:   uyuniv1.FormulaValueFrom{Path: "", Format: "yaml", ConfigMapKeyRef: cmRef},
		},
		{
			name:     "string source empty path is rejected",
			vf:       uyuniv1.FormulaValueFrom{Path: "", ConfigMapKeyRef: cmRef},
			wantErrs: 1,
			wantPath: "spec.formulas[0].valuesFrom[0].path",
		},
		{
			name:     "format with objectFieldRef is rejected",
			vf:       uyuniv1.FormulaValueFrom{Path: "x", Format: "yaml", ObjectFieldRef: objRef},
			wantErrs: 1,
			wantPath: "spec.formulas[0].valuesFrom[0].format",
		},
		{
			name:     "no source is rejected",
			vf:       uyuniv1.FormulaValueFrom{Path: "x"},
			wantErrs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := []uyuniv1.FormulaAssignment{{Name: "branch-server", ValuesFrom: []uyuniv1.FormulaValueFrom{tc.vf}}}
			errs := validation.SystemFormulas(f, field.NewPath("spec.formulas"))
			require.Len(t, errs, tc.wantErrs, "errors: %v", errs)
			if tc.wantPath != "" {
				found := false
				for _, e := range errs {
					if e.Field == tc.wantPath {
						found = true
						break
					}
				}
				require.True(t, found, "expected error at %q, got %v", tc.wantPath, errs)
			}
		})
	}
}

func TestMaintenanceCalendarSourceMutex(t *testing.T) {
	cases := []struct {
		name     string
		ical     string
		url      string
		wantErrs int
	}{
		{name: "ical only is valid", ical: "BEGIN:VCALENDAR..."},
		{name: "url only is valid", url: "https://example.com/cal.ics"},
		{name: "neither is rejected", wantErrs: 1},
		{name: "both is rejected", ical: "BEGIN:VCALENDAR...", url: "https://example.com/cal.ics", wantErrs: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validation.MaintenanceCalendarSourceMutex(tc.ical, tc.url, field.NewPath("spec"))
			require.Len(t, errs, tc.wantErrs, "errors: %v", errs)
		})
	}
}

func TestMaintenanceScheduleTargetCount(t *testing.T) {
	oneRef := []uyuniv1.LocalObjectRef{{Name: "sys-a"}}
	twoRefs := []uyuniv1.LocalObjectRef{{Name: "sys-a"}, {Name: "sys-b"}}
	groupRef := []uyuniv1.LocalObjectRef{{Name: "grp-a"}}

	cases := []struct {
		name            string
		schedType       string
		systemRefs      []uyuniv1.LocalObjectRef
		systemGroupRefs []uyuniv1.LocalObjectRef
		wantErrs        int
		wantPath        string
	}{
		{name: "multi with groups and systems is valid", schedType: "Multi", systemRefs: twoRefs, systemGroupRefs: groupRef},
		{name: "single with no targets is valid", schedType: "Single"},
		{name: "single with one system is valid", schedType: "Single", systemRefs: oneRef},
		{
			name:       "single with two systems is rejected",
			schedType:  "Single",
			systemRefs: twoRefs,
			wantErrs:   1,
			wantPath:   "spec.systemRefs",
		},
		{
			name:            "single with a group is rejected",
			schedType:       "Single",
			systemGroupRefs: groupRef,
			wantErrs:        1,
			wantPath:        "spec.systemGroupRefs",
		},
		{
			name:            "single with a group and two systems is rejected twice",
			schedType:       "Single",
			systemRefs:      twoRefs,
			systemGroupRefs: groupRef,
			wantErrs:        2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validation.MaintenanceScheduleTargetCount(
				tc.schedType, tc.systemRefs, tc.systemGroupRefs, field.NewPath("spec"))
			require.Len(t, errs, tc.wantErrs, "errors: %v", errs)
			if tc.wantPath != "" {
				found := false
				for _, e := range errs {
					if e.Field == tc.wantPath {
						found = true
						break
					}
				}
				require.True(t, found, "expected error at %q, got %v", tc.wantPath, errs)
			}
		})
	}
}
