package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

type KiwiSource struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
}

type DockerfileSource struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
}

type ImageProfileSpec struct {
	// +kubebuilder:validation:Required
	// Immutable after creation.
	Label string `json:"label"`

	// +kubebuilder:validation:Enum=kiwi;dockerfile
	// +kubebuilder:default=kiwi
	// Immutable after creation.
	Type string `json:"type"`

	// +kubebuilder:validation:Required
	StoreRef LocalObjectRef `json:"storeRef"`

	ActivationKeyRef *LocalObjectRef `json:"activationKeyRef,omitempty"`

	Kiwi       *KiwiSource       `json:"kiwi,omitempty"`
	Dockerfile *DockerfileSource `json:"dockerfile,omitempty"`

	CustomInfo map[string]string `json:"customInfo,omitempty"`

	// BuildPolicy controls automatic builds:
	//   - "manual":   only AnnBuildNow annotation triggers a build
	//   - "onChange": rebuild when spec generation changes
	//   - cron expr:  rebuild on schedule
	// +kubebuilder:default=manual
	BuildPolicy string `json:"buildPolicy,omitempty"`

	BuildHostRef *LocalObjectRef `json:"buildHostRef,omitempty"`

	OrganizationRef *LocalObjectRef `json:"organizationRef,omitempty"`
}

type ImageBuildRecord struct {
	BuildID     int          `json:"buildId,omitempty"`
	Version     string       `json:"version,omitempty"`
	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +kubebuilder:validation:Enum=Queued;Running;Succeeded;Failed
	Status        string `json:"status,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	// What initiated this build: "annotation", "onChange", "cron", "initial".
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

func init() {
	SchemeBuilder.Register(
		&ImageStore{}, &ImageStoreList{},
		&ImageProfile{}, &ImageProfileList{},
	)
}
