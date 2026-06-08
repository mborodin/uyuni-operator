package controller

import (
	"context"
	"fmt"

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
	var sc uyuniv1.SoftwareChannel
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &sc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", "", fmt.Sprintf("SoftwareChannel %q not found", ref.Name), nil
		}
		return "", "", "", err
	}
	if sc.Status.Label == "" {
		return "", fmt.Sprintf("SoftwareChannel %q not yet realized in Uyuni", ref.Name), "", nil
	}
	return sc.Status.Label, "", "", nil
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
