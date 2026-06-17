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

// BasicAuthRef references a Secret that contains username and password keys for HTTP Basic Auth.
// The controller reads the credentials at reconcile time and injects them into the source URL;
// they are never stored in status.
type BasicAuthRef struct {
	// Name is the Secret in the same namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// UsernameKey is the key for the username value. Defaults to "username".
	// +kubebuilder:default="username"
	UsernameKey string `json:"usernameKey,omitempty"`
	// PasswordKey is the key for the password value. Defaults to "password".
	// +kubebuilder:default="password"
	PasswordKey string `json:"passwordKey,omitempty"`
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
	// ContentProjectRef is optional. When its name is empty, no content
	// project channel is attached and resolution is skipped (the resource
	// reconciles without a project-derived channel) rather than erroring.
	// +optional
	ContentProjectRef LocalObjectRef `json:"contentProjectRef,omitempty"`

	// Environment is optional; required only when ContentProjectRef is set.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Environment string `json:"environment,omitempty"`

	// SourceChannelLabel is the channel label as it exists upstream of the
	// project (i.e., what's attached as a source), not the derived label.
	// +kubebuilder:validation:Required
	SourceChannelLabel string `json:"sourceChannelLabel"`
}
