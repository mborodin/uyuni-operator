package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// ChannelRefMutex validates that the two ways of specifying base/child
// channels are not mixed. Used by ActivationKey and System validators.
//
// basePath should be the spec root; this returns errors at
// "{base}.baseChannelRef" / "{base}.baseChannelFrom" etc. so the customer
// can navigate to either field.
func ChannelRefMutex(
	baseRef *uyuniv1.LocalObjectRef,
	baseFrom *uyuniv1.ChannelFromProject,
	childRefs []uyuniv1.LocalObjectRef,
	childFrom []uyuniv1.ChannelFromProject,
	basePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	if baseRef != nil && baseFrom != nil {
		errs = append(errs, field.Forbidden(basePath,
			"baseChannelRef and baseChannelFrom are mutually exclusive"))
	}
	if len(childRefs) > 0 && len(childFrom) > 0 {
		errs = append(errs, field.Forbidden(basePath,
			"childChannelRefs and childChannelsFrom are mutually exclusive"))
	}
	return errs
}

// PreCreateRequiresIdentification validates that PreCreate=true is paired
// with at least one MAC address or a hostname (one of these is required
// by Uyuni's createSystemProfile API).
func PreCreateRequiresIdentification(
	preCreate bool,
	hostname string,
	network []uyuniv1.NetworkInterface,
	basePath *field.Path,
) field.ErrorList {
	if !preCreate {
		return nil
	}
	if hostname != "" {
		return nil
	}
	for _, n := range network {
		if n.MACAddress != "" {
			return nil
		}
	}
	return field.ErrorList{field.Required(basePath,
		"preCreate requires at least one network interface with macAddress or a hostname")}
}
