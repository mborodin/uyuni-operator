package v1alpha1

// LocalObjectRef is a name-only reference to a CR in the same namespace.
// We deliberately don't include namespace or kind: cross-namespace refs
// complicate RBAC and aren't needed in our model, and each ref's kind is
// implied by the field where it appears.
type LocalObjectRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// SecretKeyRef selects a key from a Kubernetes Secret in the same namespace.
type SecretKeyRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ChannelFromProject references a channel produced by a ContentProject
// environment. Resolution is structural: the realized Uyuni channel label
// is {project.spec.label}-{environment}-{sourceChannelLabel}.
//
// Verification at reconcile time additionally checks the channel exists in
// the project's status.environmentStates[*].derivedChannels, which catches
// the case where sourceChannelLabel names a channel that isn't actually a
// source of the project.
type ChannelFromProject struct {
	// +kubebuilder:validation:Required
	ContentProjectRef LocalObjectRef `json:"contentProjectRef"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Environment string `json:"environment"`

	// SourceChannelLabel is the channel label as it exists upstream of the
	// project (i.e., what's attached as a source), not the derived label.
	// +kubebuilder:validation:Required
	SourceChannelLabel string `json:"sourceChannelLabel"`
}
