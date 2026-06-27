package uyuni

import "time"

// --- System ---

type SystemDetails struct {
	ID                 int
	Name               string
	MinionID           string
	Hostname           string
	Description        string
	ContactMethod      string
	BaseChannelLabel   string
	ChildChannelLabels []string
	// BaseEntitlement is e.g. "bootstrap_entitled" while the system is still
	// completing its first registration, or "management_entitled" etc. once
	// fully registered. Channel subscriptions and add-on entitlements cannot
	// be applied while the system is still "bootstrap_entitled".
	BaseEntitlement string
	LastCheckin     time.Time
}

// SystemDetailsUpdate carries the mutable fields written by system.setDetails.
// Only non-empty fields are sent; callers set what they want to change.
type SystemDetailsUpdate struct {
	Description   string
	ContactMethod string // "default" | "ssh-push" | "ssh-push-tunnel"
}

type SystemProfileData struct {
	HWAddress string
	Hostname  string
}

// --- SystemGroup ---

type SystemGroupDetails struct {
	ID          int
	Name        string
	Description string
	SystemCount int
}

// --- ActivationKey ---

type ActivationKeyDetails struct {
	Key              string
	Description      string
	BaseChannelLabel string
	ChildChannels    []string
	ConfigChannels   []string
	Entitlements     []string
	UsageLimit       int
	UniversalDefault bool
	Disabled         bool
	ContactMethod    string
	ServerGroupIDs   []int
}

// --- Channels & Repos ---

type ChannelDetails struct {
	ID                 int
	Label              string
	Name               string
	Summary            string
	Description        string
	ArchName           string
	ParentChannelLabel string
	ChecksumLabel      string
	GPGKeyURL          string
	GPGKeyID           string
	GPGKeyFp           string
	GPGCheck           bool
	PackageCount       int
	LastSynced         string
}

type RepoDetails struct {
	ID                int
	Label             string
	URL               string
	Type              string
	HasSignedMetadata bool
}

// --- Config channels & files ---

type ConfigChannelDetails struct {
	ID          int
	Label       string
	Name        string
	Description string
	Type        string
}

type ConfigFileDetails struct {
	Path        string
	Type        string
	Revision    int
	Contents    string
	TargetPath  string
	Owner       string
	Group       string
	Permissions string
	SELinuxCtx  string
	Macro       bool
	Binary      bool
}

type ConfigFileUpsert struct {
	Path        string
	Type        string
	Contents    string
	TargetPath  string
	Owner       string
	Group       string
	Permissions string
	SELinuxCtx  string
	Macro       bool
}

// --- Content management ---

type ProjectDetails struct {
	ID          int
	Label       string
	Name        string
	Description string
}

type ProjectSource struct {
	Channel struct {
		ID    int
		Label string
	}
	State string // ATTACHED | DETACHED | BUILT
}

type ProjectEnvironmentInfo struct {
	ID                       int
	Label                    string
	Name                     string
	Description              string
	Version                  int
	PreviousEnvironmentLabel string
	Status                   string // NEW | BUILDING | GENERATING_REPODATA | BUILT | FAILED
}

type FilterCriteriaWire struct {
	Field   string
	Matcher string
	Value   string
}

type FilterDetails struct {
	ID         int
	Name       string
	EntityType string
	Rule       string
	Criteria   FilterCriteriaWire
}

// --- Image stores / profiles ---

type ImageStoreDetails struct {
	ID    int
	Label string
	URI   string
	Type  string
}

type ImageProfileDetails struct {
	ID            int
	Label         string
	Type          string
	StoreLabel    string
	ActivationKey string
	SourcePath    string
	SourceURL     string
	SourceBranch  string
}

type ImageInfo struct {
	ID           int
	Name         string
	Version      string
	Revision     int
	BuildStatus  string
	ProfileLabel string
}

// --- Autoinstall (kickstart.tree + kickstart.profile) ---

type DistributionDetails struct {
	ID                int
	Label             string
	BasePath          string
	ChannelLabel      string
	InstallType       string
	KernelOptions     string
	PostKernelOptions string
}

// CustomInfoKeyDetails is an organization-level custom system info key
// (system.custominfo.listAllKeys).
type CustomInfoKeyDetails struct {
	ID          int    `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ProxyHop is one entry in a system's proxy connection path
// (system.getConnectionPath).
type ProxyHop struct {
	Position int    `json:"position"`
	ID       int    `json:"id"`
	Hostname string `json:"hostname"`
}

type ProfileCreateArgs struct {
	Label              string
	VirtualizationType string
	TreeLabel          string
	KickstartHost      string
	RootPassword       string
	UpdateType         string
}

type ProfileImportArgs struct {
	Label         string
	TreeLabel     string
	KickstartHost string
	Contents      string
}

type ProfileDetails struct {
	Label              string
	VirtualizationType string
	TreeLabel          string
	UpdateType         string
}

// ProfileScript describes a single pre/post script attached to an autoinstall profile.
// Name is our reconcile key — it is encoded as a "#name:<name>\n" prefix in Contents
// so it survives the round-trip through Uyuni's API, which has no separate name field.
type ProfileScript struct {
	ID          int
	Name        string
	Contents    string
	Interpreter string
	Type        string // "pre" | "post"
	Chroot      bool
	Template    bool
	ErrorOnFail bool
}

// --- Scheduled actions ---

type ScheduledAction struct {
	ID         int
	Name       string
	Type       string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
}

type SystemActionResult struct {
	ServerID int
	ActionID int
	Status   string
	Result   string
	ExitCode *int
}
