// Package uyuni contains the Uyuni JSON API client and the abstract
// API interface consumed by controllers. Both *Client (real) and
// uyunitest.FakeAPI (test) implement uyuni.API.
package uyuni

import (
	"context"
	"errors"
	"time"
)

// LocalObjectRef is a name-only reference used by the ClientPool to
// resolve a UyuniProvider CR to a configured client. Kept here (rather
// than importing the v1alpha1 types) so the uyuni package has no
// dependency on Kubernetes types — keeps the API client testable in
// isolation.
type LocalObjectRef struct {
	Name string
}

// ClientPool resolves a ProviderRef or OrganizationRef into a ready API.
// Implementations cache clients keyed by provider/org name and invalidate
// on credential changes.
type ClientPool interface {
	For(ctx context.Context, ref *LocalObjectRef, requestNamespace string) (API, error)
	ForOrganization(ctx context.Context, orgName string, orgNamespace string) (API, error)
	Invalidate(providerName string)
	InvalidateOrg(orgKey string)
	OrgID(providerName string) (int, bool)
}

// API is the full contract consumed by reconcilers. Method groups
// match Uyuni subsystems.
type API interface {
	// System
	FindSystemByMinionID(ctx context.Context, minionID string) (*SystemDetails, error)
	FindSystemByMAC(ctx context.Context, mac string) (*SystemDetails, error)
	CreateSystemProfile(ctx context.Context, name string, data SystemProfileData) (int, error)
	GetSystemDetails(ctx context.Context, serverID int) (*SystemDetails, error)
	// SetSystemDetails updates mutable system properties via system.setDetails.
	// Only fields set in d are sent; zero values are omitted.
	SetSystemDetails(ctx context.Context, serverID int, d SystemDetailsUpdate) error
	DeleteSystem(ctx context.Context, serverID int) error

	GetCustomInfo(ctx context.Context, serverID int) (map[string]string, error)
	SetCustomInfo(ctx context.Context, serverID int, kv map[string]string) error
	DeleteCustomInfo(ctx context.Context, serverID int, keys []string) error

	ScheduleChangeChannels(ctx context.Context, serverID int, base string, children []string, earliest time.Time) (int, error)

	// SetBaseChannel and SetChildChannels perform an immediate (unscheduled)
	// channel subscription. Used for systems with no current base channel —
	// system.scheduleChangeChannels requires an existing subscription to
	// schedule a change action against and rejects "Bootstrap"-type systems
	// with "No method exists with the matching parameters".
	SetBaseChannel(ctx context.Context, serverID int, label string) error
	SetChildChannels(ctx context.Context, serverID int, labels []string) error

	// ListSystemConfigChannels returns the ordered config channel label list
	// subscribed directly to the system (system.config.listChannels).
	ListSystemConfigChannels(ctx context.Context, serverID int) ([]string, error)
	// SetSystemConfigChannels replaces the system's config channel subscription
	// with the given ordered label list (system.config.setChannels).
	SetSystemConfigChannels(ctx context.Context, serverID int, channelLabels []string) error

	// ProvisionSystem schedules Cobbler/Kickstart reprovisioning for the system
	// (system.provisionSystem). Returns the Uyuni action ID.
	ProvisionSystem(ctx context.Context, serverID int, profile string, earliest time.Time) (int, error)

	ListEntitlements(ctx context.Context, serverID int) ([]string, error)
	AddEntitlements(ctx context.Context, serverID int, addons []string) (int, error)
	RemoveEntitlements(ctx context.Context, serverID int, addons []string) error

	// SystemGroup
	CreateSystemGroup(ctx context.Context, name, description string) (*SystemGroupDetails, error)
	GetSystemGroup(ctx context.Context, name string) (*SystemGroupDetails, error)
	UpdateSystemGroupDescription(ctx context.Context, name, description string) error
	DeleteSystemGroup(ctx context.Context, name string) error
	ListSystemsInGroup(ctx context.Context, name string) ([]int, error)
	AddSystemsToGroup(ctx context.Context, name string, serverIDs []int) error
	RemoveSystemsFromGroup(ctx context.Context, name string, serverIDs []int) error
	SubscribeGroupToConfigChannel(ctx context.Context, groupName, channelLabel string) error
	UnsubscribeGroupFromConfigChannel(ctx context.Context, groupName, channelLabel string) error

	// ActivationKey
	CreateActivationKey(ctx context.Context, in ActivationKeyDetails) (string, error)
	GetActivationKey(ctx context.Context, key string) (*ActivationKeyDetails, error)
	DeleteActivationKey(ctx context.Context, key string) error
	SetActivationKeyDetails(ctx context.Context, key string, d ActivationKeyDetails) error
	AddChildChannels(ctx context.Context, key string, labels []string) error
	RemoveChildChannels(ctx context.Context, key string, labels []string) error
	SetActivationKeyConfigChannels(ctx context.Context, key string, channelLabels []string) error
	SetActivationKeyGroups(ctx context.Context, key string, groupIDs []int) error

	// Channels & Repos
	CreateChannel(ctx context.Context, spec ChannelDetails) error
	GetChannel(ctx context.Context, label string) (*ChannelDetails, error)
	SetChannelDetails(ctx context.Context, id int, d ChannelDetails) error
	DeleteChannel(ctx context.Context, label string) error
	SetChannelGloballySubscribable(ctx context.Context, label string, subscribable bool) error
	ListChannelRepos(ctx context.Context, label string) ([]string, error)
	AssociateRepo(ctx context.Context, channelLabel, repoLabel string) error
	DisassociateRepo(ctx context.Context, channelLabel, repoLabel string) error
	SetRepoSyncSchedule(ctx context.Context, channelLabel, quartzCron string) error
	SyncChannelNow(ctx context.Context, channelLabel string) error

	CreateRepo(ctx context.Context, r RepoDetails, sslCa, sslCert, sslKey string) (*RepoDetails, error)
	GetRepo(ctx context.Context, label string) (*RepoDetails, error)
	UpdateRepoURL(ctx context.Context, label, url string) error
	DeleteRepo(ctx context.Context, label string) error

	// Config channels & files
	CreateConfigChannel(ctx context.Context, label, name, description, chanType string) (*ConfigChannelDetails, error)
	GetConfigChannel(ctx context.Context, label string) (*ConfigChannelDetails, error)
	UpdateConfigChannel(ctx context.Context, label, name, description string) error
	DeleteConfigChannel(ctx context.Context, label string) error
	ListConfigFiles(ctx context.Context, channelLabel string) ([]ConfigFileDetails, error)
	GetConfigFile(ctx context.Context, channelLabel, path string) (*ConfigFileDetails, error)
	CreateOrUpdateConfigFile(ctx context.Context, channelLabel string, f ConfigFileUpsert) (*ConfigFileDetails, error)
	DeleteConfigFile(ctx context.Context, channelLabel, path string) error

	// Content management
	CreateProject(ctx context.Context, label, name, description string) (*ProjectDetails, error)
	LookupProject(ctx context.Context, label string) (*ProjectDetails, error)
	UpdateProject(ctx context.Context, label, name, description string) error
	RemoveProject(ctx context.Context, label string) error

	ListProjectSources(ctx context.Context, projectLabel string) ([]ProjectSource, error)
	AttachSource(ctx context.Context, projectLabel, channelLabel string) error
	DetachSource(ctx context.Context, projectLabel, channelLabel string) error

	ListProjectEnvironments(ctx context.Context, projectLabel string) ([]ProjectEnvironmentInfo, error)
	CreateEnvironment(ctx context.Context, projectLabel, label, name, description, predecessor string) error
	UpdateEnvironment(ctx context.Context, projectLabel, envLabel, name, description string) error
	RemoveEnvironment(ctx context.Context, projectLabel, envLabel, name, description string) error

	ListFilters(ctx context.Context) ([]FilterDetails, error)
	CreateFilter(ctx context.Context, name, entityType, rule string, criteria FilterCriteriaWire) (*FilterDetails, error)
	UpdateFilter(ctx context.Context, id int, name, rule string, criteria FilterCriteriaWire) error
	RemoveFilter(ctx context.Context, id int) error
	AttachFilter(ctx context.Context, projectLabel string, id int) error
	DetachFilter(ctx context.Context, projectLabel string, id int) error

	BuildProject(ctx context.Context, projectLabel, message string) error
	PromoteProject(ctx context.Context, projectLabel, envLabel string) error

	// Autoinstall — kickstart.tree (distribution)
	CreateDistribution(ctx context.Context, d DistributionDetails) error
	GetDistribution(ctx context.Context, label string) (*DistributionDetails, error)
	UpdateDistribution(ctx context.Context, label string, d DistributionDetails) error
	DeleteDistribution(ctx context.Context, label string) error

	// Autoinstall — kickstart / kickstart.profile
	CreateProfile(ctx context.Context, args ProfileCreateArgs) error
	ImportProfile(ctx context.Context, args ProfileImportArgs) error
	GetProfile(ctx context.Context, label string) (*ProfileDetails, error)
	DeleteProfile(ctx context.Context, label string) error
	SetProfileChildChannels(ctx context.Context, label string, channelLabels []string) error
	GetProfileChildChannels(ctx context.Context, label string) ([]string, error)
	SetProfileVariables(ctx context.Context, label string, vars map[string]string) error
	GetProfileVariables(ctx context.Context, label string) (map[string]string, error)
	SetProfileUpdateType(ctx context.Context, label, updateType string) error
	SetProfileCfgPreservation(ctx context.Context, label string, preserve bool) error
	AddProfileScript(ctx context.Context, label string, s ProfileScript) (int, error)
	ListProfileScripts(ctx context.Context, label string) ([]ProfileScript, error)
	RemoveProfileScript(ctx context.Context, label string, scriptID int) error

	// Image stores / profiles
	CreateImageStore(ctx context.Context, label, storeType, uri, user, pass string) error
	GetImageStore(ctx context.Context, label string) (*ImageStoreDetails, error)
	UpdateImageStore(ctx context.Context, label, uri string) error
	DeleteImageStore(ctx context.Context, label string) error
	CreateImageProfile(ctx context.Context, p ImageProfileDetails, customInfo map[string]string) error
	GetImageProfile(ctx context.Context, label string) (*ImageProfileDetails, error)
	UpdateImageProfile(ctx context.Context, label string, details map[string]any) error
	DeleteImageProfile(ctx context.Context, label string) error
	ScheduleImageBuild(ctx context.Context, profileLabel, version string, buildHostID int) (int, error)
	ListImagesForProfile(ctx context.Context, profileLabel string) ([]ImageInfo, error)

	// Scheduled actions (tasks)
	ScheduleHighstate(ctx context.Context, serverIDs []int, earliest time.Time, test bool) (int, error)
	ScheduleRemoteCommand(ctx context.Context, serverIDs []int, earliest time.Time, command, user, group string, timeoutSeconds int) (int, error)
	ScheduleReboot(ctx context.Context, serverIDs []int, earliest time.Time) ([]int, error)
	ScheduleApplyPatches(ctx context.Context, serverIDs []int, earliest time.Time, advisoryNames []string) ([]int, error)
	ScheduleApplyConfigChannels(ctx context.Context, serverIDs []int, earliest time.Time) (int, error)
	GetActionDetails(ctx context.Context, actionID int) (*ScheduledAction, error)
	GetActionResults(ctx context.Context, actionID int) ([]SystemActionResult, error)
	CancelAction(ctx context.Context, actionID int) error

	// Organization management (satellite admin operations)
	CreateOrganization(ctx context.Context, name, adminLogin, adminPass, adminFirstName, adminLastName, adminEmail string) (int, error)
	GetOrganizationByID(ctx context.Context, id int) (*OrgDetails, error)
	GetOrganizationByName(ctx context.Context, name string) (*OrgDetails, error)
	DeleteOrganization(ctx context.Context, id int) error

	// Server-level
	GetServerVersion(ctx context.Context) (string, error)
	GetOrgID(ctx context.Context) (int, error)
}

// OrgDetails is a summary of a Uyuni organization.
type OrgDetails struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// IsNotFound reports whether the error represents a Uyuni "not found"
// fault. Both real apiError and the test fake's not-found errors
// satisfy this via the notFound interface.
func IsNotFound(err error) bool {
	var nf interface{ notFound() bool }
	if errors.As(err, &nf) {
		return nf.notFound()
	}
	return false
}

// SystemExistsError signals that createSystemProfile found an existing
// system matching the supplied hwAddress/hostname. Caller should adopt
// IDs[0] rather than treating this as failure.
type SystemExistsError struct{ IDs []int }

func (e *SystemExistsError) Error() string {
	return "system already exists"
}
