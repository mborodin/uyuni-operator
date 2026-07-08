package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// channelResolution captures the outcome of resolving direct refs and
// project-environment refs into Uyuni channel labels.
//
// HardError signals a misconfiguration that the customer must address
// (the webhook should have caught it, but admission can be bypassed and
// referenced resources can disappear at runtime).
//
// WaitReason/WaitDetail signal a transient state (e.g., environment not
// yet built) that the reconciler should requeue on. Different requeue
// cadence than HardError.
type channelResolution struct {
	BaseChannelLabel   string
	ChildChannelLabels []string

	WaitReason string
	WaitDetail string

	HardError string
}

// channelRefs captures the union of both reference styles. Reconcilers
// extract their own spec fields into this and pass to resolveChannelRefs.
type channelRefs struct {
	BaseChannelRef    *uyuniv1.LocalObjectRef
	ChildChannelRefs  []uyuniv1.LocalObjectRef
	BaseChannelFrom   *uyuniv1.ChannelFromProject
	ChildChannelsFrom []uyuniv1.ChannelFromProject
}

func resolveChannelRefs(ctx context.Context, c client.Client, namespace string, refs channelRefs) (*channelResolution, error) {
	out := &channelResolution{}

	// A ChannelFromProject with an empty contentProjectRef.name is a DIRECT
	// channel reference: sourceChannelLabel names an existing Uyuni channel
	// directly, rather than a content-project-derived one. (It arises naturally
	// because ContentProjectRef is a value field, so setting baseChannelFrom with
	// only a sourceChannelLabel leaves contentProjectRef as {name: ""}.) Resolve
	// such refs to their label as-is; a bare {name: ""} with no sourceChannelLabel
	// means "no channel" and is skipped. Uyuni validates the label exists when the
	// reconciler applies it.
	if refs.BaseChannelFrom != nil && refs.BaseChannelFrom.ContentProjectRef.Name == "" {
		if refs.BaseChannelFrom.SourceChannelLabel != "" {
			out.BaseChannelLabel = refs.BaseChannelFrom.SourceChannelLabel
		}
		refs.BaseChannelFrom = nil
	}
	if len(refs.ChildChannelsFrom) > 0 {
		filtered := make([]uyuniv1.ChannelFromProject, 0, len(refs.ChildChannelsFrom))
		for _, ref := range refs.ChildChannelsFrom {
			if ref.ContentProjectRef.Name == "" {
				if ref.SourceChannelLabel != "" {
					out.ChildChannelLabels = append(out.ChildChannelLabels, ref.SourceChannelLabel)
				}
				continue
			}
			filtered = append(filtered, ref)
		}
		refs.ChildChannelsFrom = filtered
	}

	// Defense-in-depth: webhook should reject these. If we see them, the
	// cluster's webhook configuration is broken; surface that diagnostically.
	if refs.BaseChannelRef != nil && refs.BaseChannelFrom != nil {
		out.HardError = "baseChannelRef and baseChannelFrom both set; admission should have rejected, check webhook configuration"
		return out, nil
	}
	if len(refs.ChildChannelRefs) > 0 && len(refs.ChildChannelsFrom) > 0 {
		out.HardError = "childChannelRefs and childChannelsFrom both set; admission should have rejected, check webhook configuration"
		return out, nil
	}

	// Base channel.
	switch {
	case refs.BaseChannelRef != nil:
		label, wait, hard, err := resolveDirectChannelRef(ctx, c, namespace, *refs.BaseChannelRef)
		if err != nil {
			return nil, err
		}
		if hard != "" {
			out.HardError = hard
			return out, nil
		}
		if wait != "" {
			out.WaitReason, out.WaitDetail = "WaitingForChannel", wait
			return out, nil
		}
		out.BaseChannelLabel = label

	case refs.BaseChannelFrom != nil:
		label, wait, hard, err := resolveFromProject(ctx, c, namespace, *refs.BaseChannelFrom)
		if err != nil {
			return nil, err
		}
		if hard != "" {
			out.HardError = hard
			return out, nil
		}
		if wait != "" {
			out.WaitReason, out.WaitDetail = "WaitingForEnvironmentBuild", wait
			return out, nil
		}
		out.BaseChannelLabel = label
	}

	// Child channels. Mode is implied by which list is non-empty.
	for _, ref := range refs.ChildChannelRefs {
		label, wait, hard, err := resolveDirectChannelRef(ctx, c, namespace, ref)
		if err != nil {
			return nil, err
		}
		if hard != "" {
			out.HardError = hard
			return out, nil
		}
		if wait != "" {
			out.WaitReason, out.WaitDetail = "WaitingForChannel", wait
			return out, nil
		}
		out.ChildChannelLabels = append(out.ChildChannelLabels, label)
	}
	for _, ref := range refs.ChildChannelsFrom {
		label, wait, hard, err := resolveFromProject(ctx, c, namespace, ref)
		if err != nil {
			return nil, err
		}
		if hard != "" {
			out.HardError = hard
			return out, nil
		}
		if wait != "" {
			out.WaitReason, out.WaitDetail = "WaitingForEnvironmentBuild", wait
			return out, nil
		}
		out.ChildChannelLabels = append(out.ChildChannelLabels, label)
	}

	return out, nil
}

func resolveDirectChannelRef(ctx context.Context, c client.Client, namespace string, ref uyuniv1.LocalObjectRef) (label, waitDetail, hardError string, err error) {
	sc, err := findSoftwareChannel(ctx, c, namespace, ref.Name)
	if err != nil {
		return "", "", "", err
	}
	if sc == nil {
		return "", "", fmt.Sprintf("SoftwareChannel %q not found", ref.Name), nil
	}
	if sc.Status.Label == "" {
		return "", fmt.Sprintf("SoftwareChannel %q not yet realized in Uyuni", ref.Name), "", nil
	}
	return sc.Status.Label, "", "", nil
}

// findSoftwareChannel resolves a ref string to a SoftwareChannel CR in
// namespace, trying each of the following in order and returning the first
// match. No prefix is required — a caller just writes whichever identifier
// is convenient (a same-claim short name, another claim's Uyuni label, or
// its Uyuni display name) and this tries all of them:
//
//  1. Exact Kubernetes object metadata.name.
//  2. Suffix match on metadata.name (e.g. "opensuse-leap-16-0-x86-64"
//     matches "gmrc-pzcpz-opensuse-leap-16-0-x86-64") — lets a claim use its
//     own short ref name, or another claim's, without knowing the
//     generated composite-name prefix.
//  3. spec.label (the Uyuni label) — stable across claims/composites and
//     across composite recreation, since it doesn't depend on any
//     generated Kubernetes name.
//  4. spec.name (the Uyuni WebUI display name) — the most human-friendly
//     identifier, but the least strictly unique of the three (Uyuni
//     rejects a duplicate display name on create, so in practice one
//     realized match is expected, same caveat as spec.label).
//
// An explicit "label:<value>" or "name:<value>" prefix skips straight to
// step 3 or 4, bypassing 1/2 — useful only to disambiguate if a value could
// otherwise be confused for a Kubernetes name fragment.
func findSoftwareChannel(ctx context.Context, c client.Client, namespace, name string) (*uyuniv1.SoftwareChannel, error) {
	if label, ok := strings.CutPrefix(name, "label:"); ok {
		return findSoftwareChannelByLabel(ctx, c, namespace, label)
	}
	if displayName, ok := strings.CutPrefix(name, "name:"); ok {
		return findSoftwareChannelByDisplayName(ctx, c, namespace, displayName)
	}

	var sc uyuniv1.SoftwareChannel
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sc); err == nil {
		return &sc, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	var list uyuniv1.SoftwareChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	suffix := "-" + name
	for i := range list.Items {
		if strings.HasSuffix(list.Items[i].Name, suffix) {
			return &list.Items[i], nil
		}
	}

	if byLabel, err := findSoftwareChannelByLabel(ctx, c, namespace, name); err != nil {
		return nil, err
	} else if byLabel != nil {
		return byLabel, nil
	}
	return findSoftwareChannelByDisplayName(ctx, c, namespace, name)
}

// findSoftwareChannelByLabel returns the SoftwareChannel CR in namespace whose
// spec.label matches. Uyuni labels are effectively unique per organization —
// the reconciler's ChannelLabelConflict check (see SoftwareChannelReconciler)
// already prevents two CRs from claiming the same label — so at most one
// match is expected.
func findSoftwareChannelByLabel(ctx context.Context, c client.Client, namespace, label string) (*uyuniv1.SoftwareChannel, error) {
	var list uyuniv1.SoftwareChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Label == label {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// findSoftwareChannelByDisplayName returns the SoftwareChannel CR in namespace
// whose spec.name (Uyuni display name) matches. Prefers a realized match
// (Status.UyuniID != 0) when more than one CR shares the display name — e.g.
// a duplicate that lost the ChannelLabelConflict race — so an ambiguous
// suffix/name lookup doesn't pin a ref to the broken, unrealized copy (see
// the "external:" suffix-match ambiguity this same pattern hit for
// findSystemGroup).
func findSoftwareChannelByDisplayName(ctx context.Context, c client.Client, namespace, displayName string) (*uyuniv1.SoftwareChannel, error) {
	var list uyuniv1.SoftwareChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var fallback *uyuniv1.SoftwareChannel
	for i := range list.Items {
		if list.Items[i].Spec.Name != displayName {
			continue
		}
		if list.Items[i].Status.UyuniID != 0 {
			return &list.Items[i], nil
		}
		if fallback == nil {
			fallback = &list.Items[i]
		}
	}
	return fallback, nil
}

// findSystemGroup resolves a ref string to a SystemGroup CR in namespace,
// mirroring findSoftwareChannel's no-prefix-required resolution chain:
//
//  1. Exact Kubernetes object metadata.name.
//  2. Suffix match on metadata.name (e.g. "branchservers" matches
//     "gmrc-5vtl5-branchservers") — lets a claim use its own short ref name,
//     or another claim's, without knowing the generated composite-name
//     prefix.
//  3. spec.name (the Uyuni group name) — prefers a realized match
//     (Status.UyuniID != 0) if more than one CR shares it, so an ambiguous
//     lookup doesn't pin the ref to a broken/unrealized duplicate that lost
//     the GroupNameConflict race.
//
// An explicit "external:<full-k8s-name>" ref (stripped by the Composition
// before reaching here) is just a plain name that flows through steps 1/2.
func findSystemGroup(ctx context.Context, c client.Client, namespace, name string) (*uyuniv1.SystemGroup, error) {
	var sg uyuniv1.SystemGroup
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sg); err == nil {
		return &sg, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	var list uyuniv1.SystemGroupList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	suffix := "-" + name
	for i := range list.Items {
		if strings.HasSuffix(list.Items[i].Name, suffix) {
			return &list.Items[i], nil
		}
	}

	var byName *uyuniv1.SystemGroup
	for i := range list.Items {
		if list.Items[i].Spec.Name != name {
			continue
		}
		if list.Items[i].Status.UyuniID != 0 {
			return &list.Items[i], nil
		}
		if byName == nil {
			byName = &list.Items[i]
		}
	}
	return byName, nil
}

func resolveFromProject(ctx context.Context, c client.Client, namespace string, ref uyuniv1.ChannelFromProject) (label, waitDetail, hardError string, err error) {
	var cp uyuniv1.ContentProject
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.ContentProjectRef.Name}, &cp); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", "", fmt.Sprintf("ContentProject %q not found", ref.ContentProjectRef.Name), nil
		}
		return "", "", "", err
	}

	// Env declared? Environments are managed via separate ClmEnvironment CRs
	// (cp.Spec.Environments is legacy and stays empty under that model), so
	// look for a ClmEnvironment in this namespace pointing at the project
	// with a matching id. (Hard error — likely a typo; the webhook can't
	// fully validate this since admission can be bypassed.)
	var envs uyuniv1.ClmEnvironmentList
	if err := c.List(ctx, &envs, client.InNamespace(namespace)); err != nil {
		return "", "", "", err
	}
	envDeclared := false
	for _, e := range cp.Spec.Environments {
		if e.Label == ref.Environment {
			envDeclared = true
			break
		}
	}
	if !envDeclared {
		for _, e := range envs.Items {
			if e.Spec.ProjectRef.Name == cp.Name && e.Spec.Id == ref.Environment {
				envDeclared = true
				break
			}
		}
	}
	if !envDeclared {
		return "", "", fmt.Sprintf(
			"environment %q not declared in ContentProject %q",
			ref.Environment, cp.Name), nil
	}

	// Env built yet? (Wait — project reconciler will get there.)
	var state *uyuniv1.EnvironmentState
	for i := range cp.Status.EnvironmentStates {
		if cp.Status.EnvironmentStates[i].Label == ref.Environment {
			state = &cp.Status.EnvironmentStates[i]
			break
		}
	}
	if state == nil || state.BuiltVersion == 0 {
		return "", fmt.Sprintf(
			"environment %q of ContentProject %q has not been built yet",
			ref.Environment, cp.Name), "", nil
	}

	// Source actually attached and reflected in derived channels? (Hard — the
	// referenced source isn't part of the project, build won't include it.)
	expected := fmt.Sprintf("%s-%s-%s", cp.Spec.Label, ref.Environment, ref.SourceChannelLabel)
	for _, derived := range state.DerivedChannels {
		if derived == expected {
			return expected, "", "", nil
		}
	}
	return "", "", fmt.Sprintf(
		"source channel %q not in environment %q of ContentProject %q (current derived channels: %v); "+
			"add it to the project's sourceRefs and rebuild",
		ref.SourceChannelLabel, ref.Environment, cp.Name, state.DerivedChannels), nil
}
