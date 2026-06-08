package controller

import (
	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// toProviderRef converts a v1alpha1 LocalObjectRef to the uyuni package's
// equivalent. Used only by UyuniProviderReconciler, which still speaks in
// terms of providers directly. All other reconcilers use orgRef + ForOrganization.
func toProviderRef(ref *uyuniv1.LocalObjectRef) *uyuni.LocalObjectRef {
	if ref == nil {
		return nil
	}
	return &uyuni.LocalObjectRef{Name: ref.Name}
}

// orgRef returns the name from an optional OrganizationRef. An empty string
// causes Pool.ForOrganization to return an error (organizationRef required).
func orgRef(ref *uyuniv1.LocalObjectRef) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// Finalizer strings, all rooted in the current API group. Centralized so
// reconcilers reference these constants rather than hardcoding strings.
const (
	sysFinalizer     = uyuniv1.Group + "/system"
	sgFinalizer      = uyuniv1.Group + "/systemgroup"
	akFinalizer      = uyuniv1.Group + "/activationkey"
	scFinalizer      = uyuniv1.Group + "/softwarechannel"
	repoFinalizer    = uyuniv1.Group + "/repository"
	ccFinalizer      = uyuniv1.Group + "/configchannel"
	confChanFinalizer = uyuniv1.Group + "/configurationchannel"
	cfFinalizer      = uyuniv1.Group + "/configfile"
	cpFinalizer      = uyuniv1.Group + "/contentproject"
	isFinalizer      = uyuniv1.Group + "/imagestore"
	ipFinalizer      = uyuniv1.Group + "/imageprofile"
	taskFinalizer    = uyuniv1.Group + "/task"
	provFinalizer    = uyuniv1.Group + "/uyuniprovider"
	orgFinalizer     = uyuniv1.Group + "/organization"
	clmEnvFinalizer  = uyuniv1.Group + "/clmenvironment"
	adFinalizer      = uyuniv1.Group + "/autoinstalldistribution"
	apFinalizer      = uyuniv1.Group + "/autoinstallprofile"
	ibFinalizer      = uyuniv1.Group + "/imagebuild"
)
