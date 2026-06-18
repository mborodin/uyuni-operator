package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// =============================================================================
// ImageStore
// =============================================================================

type ImageStoreSpec struct {
	// +kubebuilder:validation:Required
	Label string `json:"label"`

	// +kubebuilder:validation:Enum=registry;os_image
	// +kubebuilder:default=registry
	// Immutable after creation.
	Type string `json:"type"`

	// +kubebuilder:validation:Required
	URI string `json:"uri"`

	CredentialsSecretRef *LocalObjectRef `json:"credentialsSecretRef,omitempty"`
	OrganizationRef      *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ImageStoreStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="URI",type=string,JSONPath=`.spec.uri`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type ImageStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ImageStoreSpec   `json:"spec,omitempty"`
	Status            ImageStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ImageStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageStore `json:"items"`
}

// =============================================================================
// ImageProfile
// =============================================================================

// GitSource describes a git repository as the source for an image build.
// The final URL passed to Uyuni is reconstructed as:
//
//	<repository>                    — neither Reference nor Path set
//	<repository>#<reference>        — Reference only
//	<repository>#<reference>:<path> — both set
//	<repository>#:<path>            — Path only (uses default branch)
type GitSource struct {
	// Repository is the base URL of the git repository.
	// +kubebuilder:validation:Required
	Repository string `json:"repository"`

	// Reference is the branch, tag, or commit. Appended as "#reference".
	Reference string `json:"reference,omitempty"`

	// Path is the subdirectory within the repo containing the Dockerfile or KIWI config.
	// Appended as ":path". Requires Reference to also be set (webhook enforces).
	Path string `json:"path,omitempty"`
}

type ImageProfileSpec struct {
	// Label is the Uyuni image profile label. Immutable after creation.
	// +kubebuilder:validation:Required
	Label string `json:"label"`

	// Type is the image build type. Immutable after creation.
	// +kubebuilder:validation:Enum=kiwi;dockerfile
	// +kubebuilder:default=kiwi
	Type string `json:"type"`

	// StoreRef references the ImageStore CR that holds the built image.
	// +kubebuilder:validation:Required
	StoreRef LocalObjectRef `json:"storeRef"`

	// ActivationKeyRef references the ActivationKey CR whose realized key is passed to Uyuni.
	ActivationKeyRef *LocalObjectRef `json:"activationKeyRef,omitempty"`

	// URL is the direct source URL for the image build. Mutually exclusive with Git.
	// Exactly one of URL or Git must be set.
	URL string `json:"url,omitempty"`

	// Git is a structured git source. Mutually exclusive with URL.
	// Exactly one of URL or Git must be set.
	Git *GitSource `json:"git,omitempty"`

	// Auth injects username/password Basic Auth into the source URL (URL or Git.Repository)
	// at reconcile time. Credentials are read from the named Secret and never stored in status.
	Auth *BasicAuthRef `json:"auth,omitempty"`

	CustomInfo map[string]string `json:"customInfo,omitempty"`

	// BuildPolicy controls automatic builds:
	//   - "manual":   only AnnBuildNow annotation triggers a build
	//   - "onChange": rebuild when spec generation changes
	// +kubebuilder:default=manual
	BuildPolicy string `json:"buildPolicy,omitempty"`

	// BuildHostRef references the System CR to use as the build host.
	BuildHostRef *LocalObjectRef `json:"buildHostRef,omitempty"`

	// +kubebuilder:validation:Required
	OrganizationRef *LocalObjectRef `json:"organizationRef"`
}

type ImageBuildRecord struct {
	BuildID     int          `json:"buildId,omitempty"`
	Version     string       `json:"version,omitempty"`
	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +kubebuilder:validation:Enum=Queued;Running;Succeeded;Failed
	Status        string `json:"status,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	// What initiated this build: "annotation", "onChange", "initial".
	Trigger string `json:"trigger,omitempty"`
}

type ImageProfileStatus struct {
	UyuniID            int                `json:"uyuniId,omitempty"`
	LastBuild          *ImageBuildRecord  `json:"lastBuild,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="LastBuild",type=string,JSONPath=`.status.lastBuild.status`
// +kubebuilder:printcolumn:name="BuildTime",type=date,JSONPath=`.status.lastBuild.completedAt`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type ImageProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ImageProfileSpec   `json:"spec,omitempty"`
	Status            ImageProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ImageProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageProfile `json:"items"`
}

// =============================================================================
// ImageBuild
// =============================================================================

type ImageBuildSpec struct {
	// ProfileRef references the ImageProfile CR. Immutable after the first build is scheduled.
	// +kubebuilder:validation:Required
	ProfileRef LocalObjectRef `json:"profileRef"`

	// Version is the image tag to apply to this build.
	// Auto-generated as "YYYYMMDD-HHMM" when empty.
	Version string `json:"version,omitempty"`

	// BuildHostRef overrides the ImageProfile's buildHostRef for this specific build.
	BuildHostRef *LocalObjectRef `json:"buildHostRef,omitempty"`

	// Earliest is the optional earliest time at which Uyuni will start the build action.
	Earliest *metav1.Time `json:"earliest,omitempty"`
}

type ImageBuildStatus struct {
	// ActionID is the Uyuni action ID returned by ScheduleImageBuild.
	ActionID int `json:"actionId,omitempty"`

	// ImageID is the Uyuni image record ID after the build completes successfully.
	ImageID int `json:"imageId,omitempty"`

	// Version records the version string actually used when the build was scheduled.
	Version string `json:"version,omitempty"`

	// BuildStatus is the current build state.
	// +kubebuilder:validation:Enum=Scheduled;Running;Succeeded;Failed
	BuildStatus        string             `json:"buildStatus,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profileRef.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.buildStatus`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type ImageBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ImageBuildSpec   `json:"spec,omitempty"`
	Status            ImageBuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ImageBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&ImageStore{}, &ImageStoreList{},
		&ImageProfile{}, &ImageProfileList{},
		&ImageBuild{}, &ImageBuildList{},
	)
}
