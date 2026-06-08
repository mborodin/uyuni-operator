package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type SystemReconciler struct {
	client.Client
	Clients uyuni.ClientPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systems/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=systemgroups;configurationchannels;softwarechannels;contentprojects,verbs=get;list;watch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=organizations,verbs=get;list;watch

func (r *SystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sys uyuniv1.System
	if err := r.Get(ctx, req.NamespacedName, &sys); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if migrateAnnotations(&sys) {
		return ctrl.Result{}, r.Update(ctx, &sys)
	}

	uc, err := r.Clients.ForOrganization(ctx, orgRef(sys.Spec.OrganizationRef), sys.Namespace)
	if err != nil {
		return r.fail(ctx, &sys, "OrganizationError", err)
	}

	if !sys.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uc, &sys)
	}
	if ensureFinalizer(&sys, sysFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &sys)
	}

	// Find the system in Uyuni, adopting by minionID or MAC if needed.
	details, found, err := r.findOrAdopt(ctx, uc, &sys)
	if err != nil {
		return r.fail(ctx, &sys, "ProviderError", err)
	}

	if !found {
		return r.handleNotRegistered(ctx, uc, &sys)
	}

	// Ensure serverID is persisted before applying config (handles adoption).
	if sys.Status.UyuniServerID != details.ID {
		sys.Status.UyuniServerID = details.ID
		if err := r.Status().Update(ctx, &sys); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.applyConfig(ctx, uc, &sys, details)
}

// findOrAdopt locates the system in Uyuni using three probes in order:
// 1. Known serverID in status (fast path).
// 2. Lookup by minionID (Salt key).
// 3. Lookup by MAC address (for pre-created profiles awaiting first boot).
func (r *SystemReconciler) findOrAdopt(ctx context.Context, uc uyuni.API, sys *uyuniv1.System) (*uyuni.SystemDetails, bool, error) {
	if sys.Status.UyuniServerID != 0 {
		d, err := uc.GetSystemDetails(ctx, sys.Status.UyuniServerID)
		if err == nil {
			return d, true, nil
		}
		if !uyuni.IsNotFound(err) {
			return nil, false, err
		}
		// Uyuni record disappeared; clear cached ID and fall through.
		sys.Status.UyuniServerID = 0
	}

	d, err := uc.FindSystemByMinionID(ctx, sys.Spec.MinionID)
	if err == nil {
		return d, true, nil
	}
	if !uyuni.IsNotFound(err) {
		return nil, false, err
	}

	if sys.Spec.PreCreate {
		for _, nic := range sys.Spec.Network {
			if nic.MACAddress == "" {
				continue
			}
			d, err := uc.FindSystemByMAC(ctx, nic.MACAddress)
			if err == nil {
				return d, true, nil
			}
			if !uyuni.IsNotFound(err) {
				return nil, false, err
			}
		}
	}

	return nil, false, nil
}

// handleNotRegistered handles the case where the system is not yet in Uyuni.
// It creates a pre-create profile if configured, checks the adoption timeout,
// and schedules autoinstall once the profile exists.
func (r *SystemReconciler) handleNotRegistered(ctx context.Context, uc uyuni.API, sys *uyuniv1.System) (ctrl.Result, error) {
	now := r.Now()

	// Phase: Pending → PreProvisioned (pre-create the profile).
	if sys.Spec.PreCreate && sys.Status.Phase != "PreProvisioned" && sys.Status.Phase != "Reprovisioning" {
		hwAddr := ""
		for _, nic := range sys.Spec.Network {
			if nic.MACAddress != "" {
				hwAddr = nic.MACAddress
				break
			}
		}
		serverID, err := uc.CreateSystemProfile(ctx, sys.Spec.Hostname, uyuni.SystemProfileData{
			HWAddress: hwAddr,
			Hostname:  sys.Spec.Hostname,
		})
		if err != nil {
			var existsErr *uyuni.SystemExistsError
			if errors.As(err, &existsErr) && len(existsErr.IDs) > 0 {
				// Already exists — adopt it.
				serverID = existsErr.IDs[0]
			} else {
				return r.fail(ctx, sys, "CreateFailed", err)
			}
		}
		sys.Status.UyuniServerID = serverID
		t := metav1.NewTime(now)
		sys.Status.Phase = "PreProvisioned"
		sys.Status.PhaseTransitionTime = &t
		setCondition(&sys.Status.Conditions, condPreProvisioned, metav1.ConditionTrue, sys.Generation,
			"PreProvisioned", "system profile created; waiting for minion registration")
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"WaitingForRegistration", "system profile pre-created; waiting for first registration")
		if err := r.Status().Update(ctx, sys); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Phase: PreProvisioned / Reprovisioning — check autoinstall scheduling.
	if sys.Status.UyuniServerID != 0 && sys.Spec.Autoinstall != nil && sys.Status.AutoinstallActionID == 0 {
		profileLabel, waitReason, err := r.resolveProfileLabel(ctx, sys)
		if err != nil {
			return r.fail(ctx, sys, "ResolveProfileFailed", err)
		}
		if waitReason != "" {
			setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse, "WaitingForProfile", waitReason)
			_ = r.Status().Update(ctx, sys)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		earliest := now
		if sys.Spec.Autoinstall.Earliest != nil {
			earliest = sys.Spec.Autoinstall.Earliest.Time
		}
		actionID, err := uc.ProvisionSystem(ctx, sys.Status.UyuniServerID, profileLabel, earliest)
		if err != nil {
			return r.fail(ctx, sys, "ScheduleFailed", err)
		}
		sys.Status.AutoinstallActionID = actionID
		sys.Status.AutoinstallStatus = "Scheduled"
		setCondition(&sys.Status.Conditions, condAutoinstallScheduled, metav1.ConditionTrue, sys.Generation,
			"Scheduled", fmt.Sprintf("autoinstall action %d scheduled for profile %q", actionID, profileLabel))
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"WaitingForRegistration", "autoinstall scheduled; waiting for system to re-register")
		if err := r.Status().Update(ctx, sys); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Adoption timeout check.
	if sys.Status.PhaseTransitionTime != nil && !sys.Status.PhaseTransitionTime.IsZero() {
		deadline := sys.Status.PhaseTransitionTime.Time.Add(sys.Spec.AdoptionTimeout.Duration)
		if now.After(deadline) {
			setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
				"AdoptionTimedOut", fmt.Sprintf("system did not register within %s", sys.Spec.AdoptionTimeout))
			_ = r.Status().Update(ctx, sys)
			return ctrl.Result{}, nil
		}
	} else if sys.Spec.PreCreate {
		// PhaseTransitionTime not set yet (first reconcile without pre-create happening).
		t := metav1.NewTime(now)
		sys.Status.PhaseTransitionTime = &t
		sys.Status.Phase = "Pending"
		_ = r.Status().Update(ctx, sys)
	}

	setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
		"WaitingForRegistration", "waiting for minion to register with Uyuni")
	if err := r.Status().Update(ctx, sys); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// applyConfig pushes all desired configuration to a registered system.
func (r *SystemReconciler) applyConfig(ctx context.Context, uc uyuni.API, sys *uyuniv1.System, current *uyuni.SystemDetails) (ctrl.Result, error) {
	// Handle reinstall-now annotation before applying steady-state config.
	if sys.Annotations[uyuniv1.AnnReinstallNow] == "true" {
		if sys.Spec.Autoinstall == nil {
			// Webhook should prevent this; this is a backstop.
			setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
				"CreateFailed", "admission should have rejected: reinstall-now requires spec.autoinstall")
			_ = r.Status().Update(ctx, sys)
			return ctrl.Result{}, nil
		}
		profileLabel, waitReason, err := r.resolveProfileLabel(ctx, sys)
		if err != nil {
			return r.fail(ctx, sys, "ResolveProfileFailed", err)
		}
		if waitReason != "" {
			setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse, "WaitingForProfile", waitReason)
			_ = r.Status().Update(ctx, sys)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		earliest := r.Now()
		if sys.Spec.Autoinstall.Earliest != nil {
			earliest = sys.Spec.Autoinstall.Earliest.Time
		}
		actionID, err := uc.ProvisionSystem(ctx, sys.Status.UyuniServerID, profileLabel, earliest)
		if err != nil {
			return r.fail(ctx, sys, "ScheduleFailed", err)
		}
		t := metav1.NewTime(r.Now())
		sys.Status.AutoinstallActionID = actionID
		sys.Status.AutoinstallStatus = "Scheduled"
		sys.Status.Phase = "Reprovisioning"
		sys.Status.PhaseTransitionTime = &t
		setCondition(&sys.Status.Conditions, condAutoinstallScheduled, metav1.ConditionTrue, sys.Generation,
			"Scheduled", fmt.Sprintf("reinstall action %d scheduled for profile %q", actionID, profileLabel))
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"WaitingForRegistration", "reprovisioning scheduled; waiting for system to re-register")
		if err := r.Status().Update(ctx, sys); err != nil {
			return ctrl.Result{}, err
		}
		// Strip the annotation in a separate update after status is durable.
		delete(sys.Annotations, uyuniv1.AnnReinstallNow)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Update(ctx, sys)
	}

	// 1. System details (description, contact method).
	// Each field is included in the update only when it actually needs to
	// change — Uyuni rejects any setDetails call that carries contact_method
	// for Salt-managed systems, even when the value matches the current one.
	wantContact := sys.Spec.ContactMethod
	if wantContact == "" {
		wantContact = "default"
	}
	var detailsUpdate uyuni.SystemDetailsUpdate
	needsUpdate := false
	if current.Description != sys.Spec.Description {
		detailsUpdate.Description = sys.Spec.Description
		needsUpdate = true
	}
	if current.ContactMethod != wantContact {
		detailsUpdate.ContactMethod = wantContact
		needsUpdate = true
	}
	if needsUpdate {
		if err := uc.SetSystemDetails(ctx, sys.Status.UyuniServerID, detailsUpdate); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}

	// 2. Software channels.
	res, err := resolveChannelRefs(ctx, r.Client, sys.Namespace, channelRefs{
		BaseChannelRef:    sys.Spec.BaseChannelRef,
		ChildChannelRefs:  sys.Spec.ChildChannelRefs,
		BaseChannelFrom:   sys.Spec.BaseChannelFrom,
		ChildChannelsFrom: sys.Spec.ChildChannelsFrom,
	})
	if err != nil {
		return r.fail(ctx, sys, "ResolveRefs", err)
	}
	if res.HardError != "" {
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"ReferenceUnavailable", res.HardError)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
	}
	if res.WaitReason != "" {
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			res.WaitReason, res.WaitDetail)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
	}

	// Record the resolved/desired channels in status up front — independent of
	// whether we can issue the Uyuni-side calls yet (e.g. while still waiting
	// for the minion to leave its bootstrap base entitlement, see below) — so
	// status always reflects what the System CR is converging towards.
	sys.Status.BaseChannelLabel = res.BaseChannelLabel
	sys.Status.ChildChannelLabels = res.ChildChannelLabels

	if current.BaseChannelLabel == "" {
		// No current subscription (e.g. freshly registered "Bootstrap" system) —
		// scheduleChangeChannels has nothing to schedule a change against and
		// rejects these with "No method exists with the matching parameters".
		// Subscribe directly instead.
		//
		// While the system is still on the temporary "bootstrap_entitled" base
		// entitlement, the minion hasn't completed its first check-in yet, so
		// Uyuni schedules the subscription as an action that the client can
		// never execute — it ends up "Failed" in the system's history. Wait
		// until the minion finishes registering before issuing the call, to
		// avoid spamming Uyuni with one doomed action per reconcile.
		if current.BaseEntitlement == "bootstrap_entitled" {
			setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
				"WaitingForBaseEntitlement", "system has not completed registration yet (still on bootstrap base entitlement); cannot subscribe to software channels")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
		}
		if res.BaseChannelLabel != "" && res.BaseChannelLabel != current.BaseChannelLabel {
			if err := uc.SetBaseChannel(ctx, sys.Status.UyuniServerID, res.BaseChannelLabel); err != nil {
				return r.fail(ctx, sys, "UpdateFailed", err)
			}
		}
		if !stringSlicesEqual(current.ChildChannelLabels, res.ChildChannelLabels) {
			if err := uc.SetChildChannels(ctx, sys.Status.UyuniServerID, res.ChildChannelLabels); err != nil {
				return r.fail(ctx, sys, "UpdateFailed", err)
			}
		}
	} else if current.BaseChannelLabel != res.BaseChannelLabel ||
		!stringSlicesEqual(current.ChildChannelLabels, res.ChildChannelLabels) {
		if _, err := uc.ScheduleChangeChannels(ctx, sys.Status.UyuniServerID,
			res.BaseChannelLabel, res.ChildChannelLabels, r.Now()); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}

	// 3. Config channels (direct first, then group-sourced).
	desiredCCs, ccWait, err := r.resolveOrderedConfigChannels(ctx, sys)
	if err != nil {
		return r.fail(ctx, sys, "ResolveRefs", err)
	}
	if ccWait != "" {
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"WaitingForConfigChannel", ccWait)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
	}
	if !stringSlicesEqual(sys.Status.ConfigChannelLabels, desiredCCs) {
		if err := uc.SetSystemConfigChannels(ctx, sys.Status.UyuniServerID, desiredCCs); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	sys.Status.ConfigChannelLabels = desiredCCs

	// 4. Group membership.
	desiredGroups, groupWait, err := r.resolveGroupMembership(ctx, sys)
	if err != nil {
		return r.fail(ctx, sys, "ResolveRefs", err)
	}
	if groupWait != "" {
		setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
			"WaitingForSystemGroup", groupWait)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
	}
	addGroups, rmGroups := diffStringSets(sys.Status.GroupNames, desiredGroups)
	for _, name := range addGroups {
		if err := uc.AddSystemsToGroup(ctx, name, []int{sys.Status.UyuniServerID}); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	for _, name := range rmGroups {
		if err := uc.RemoveSystemsFromGroup(ctx, name, []int{sys.Status.UyuniServerID}); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	sys.Status.GroupNames = desiredGroups

	// 5. Add-ons (entitlements).
	currentAddOns, err := uc.ListEntitlements(ctx, sys.Status.UyuniServerID)
	if err != nil && !uyuni.IsNotFound(err) {
		return r.fail(ctx, sys, "ProviderError", err)
	}
	addOns, rmOns := diffStringSets(currentAddOns, sys.Spec.AddOns)
	if len(addOns) > 0 {
		if _, err := uc.AddEntitlements(ctx, sys.Status.UyuniServerID, addOns); err != nil {
			if strings.Contains(err.Error(), "Invalid entitlement") {
				// Uyuni rejects add-on entitlements while the system still
				// carries the "bootstrap_entitled" base entitlement — it
				// upgrades to a real base entitlement (e.g. management) once
				// the minion completes its first full registration/checkin.
				setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse,
					"WaitingForBaseEntitlement", "system has not completed registration yet (still on bootstrap base entitlement); cannot grant add-on entitlements")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, sys)
			}
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	if len(rmOns) > 0 {
		if err := uc.RemoveEntitlements(ctx, sys.Status.UyuniServerID, rmOns); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	sys.Status.ActiveAddOns = sys.Spec.AddOns

	// 6. Custom info.
	currentInfo, err := uc.GetCustomInfo(ctx, sys.Status.UyuniServerID)
	if err != nil && !uyuni.IsNotFound(err) {
		return r.fail(ctx, sys, "ProviderError", err)
	}
	setKV, delKeys := diffCustomInfo(currentInfo, sys.Spec.CustomInfo)
	if len(setKV) > 0 {
		if err := uc.SetCustomInfo(ctx, sys.Status.UyuniServerID, setKV); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	if len(delKeys) > 0 {
		if err := uc.DeleteCustomInfo(ctx, sys.Status.UyuniServerID, delKeys); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}

	// Update autoinstall status if an action is in flight.
	if sys.Status.AutoinstallActionID != 0 && sys.Status.AutoinstallStatus == "Scheduled" {
		action, err := uc.GetActionDetails(ctx, sys.Status.AutoinstallActionID)
		if err == nil && action.Status != "" && action.Status != "Queued" && action.Status != "InProgress" {
			if action.Status == "Completed" {
				sys.Status.AutoinstallStatus = "Completed"
			} else {
				sys.Status.AutoinstallStatus = "Failed"
			}
		}
	}

	t := current.LastCheckin
	if !t.IsZero() {
		mt := metav1.NewTime(t)
		sys.Status.LastCheckinTime = &mt
	}

	// Apply high state once, the moment the system completes registration and
	// reaches full reconciliation for the first time — applies all assigned
	// states (channels, config channels, formulas, etc.) immediately instead
	// of waiting for the next scheduled highstate run.
	if sys.Spec.ApplyHighState && sys.Status.Phase != "Reconciled" {
		if _, err := uc.ScheduleHighstate(ctx, []int{sys.Status.UyuniServerID}, r.Now(), false); err != nil {
			return r.fail(ctx, sys, "UpdateFailed", err)
		}
	}
	sys.Status.Phase = "Reconciled"
	sys.Status.ObservedGeneration = sys.Generation
	setCondition(&sys.Status.Conditions, condPreProvisioned, metav1.ConditionFalse, sys.Generation,
		"Registered", "system is registered and managed")
	setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, sys); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *SystemReconciler) handleDeletion(ctx context.Context, uc uyuni.API, sys *uyuniv1.System) (ctrl.Result, error) {
	if !containsFinalizer(sys, sysFinalizer) {
		return ctrl.Result{}, nil
	}
	if sys.Annotations[uyuniv1.AnnForceDelete] == "true" {
		removeFinalizer(sys, sysFinalizer)
		return ctrl.Result{}, r.Update(ctx, sys)
	}
	if sys.Status.UyuniServerID != 0 {
		// Remove from all groups before deleting.
		for _, name := range sys.Status.GroupNames {
			_ = uc.RemoveSystemsFromGroup(ctx, name, []int{sys.Status.UyuniServerID})
		}
		if sys.Spec.DeletionPolicy == "Delete" {
			if err := uc.DeleteSystem(ctx, sys.Status.UyuniServerID); err != nil && !uyuni.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	removeFinalizer(sys, sysFinalizer)
	return ctrl.Result{}, r.Update(ctx, sys)
}

// resolveOrderedConfigChannels builds the prioritized ordered config channel
// label list: direct system refs first, then channels from each groupRef.
func (r *SystemReconciler) resolveOrderedConfigChannels(ctx context.Context, sys *uyuniv1.System) (labels []string, wait string, err error) {
	seen := map[string]bool{}

	for _, ref := range sys.Spec.ConfigChannelRefs {
		var cc uyuniv1.ConfigurationChannel
		if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ref.Name}, &cc); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("ConfigurationChannel %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if cc.Status.UyuniID == 0 {
			return nil, fmt.Sprintf("ConfigurationChannel %q not yet realized", ref.Name), nil
		}
		if !seen[cc.Spec.ID] {
			labels = append(labels, cc.Spec.ID)
			seen[cc.Spec.ID] = true
		}
	}

	for _, groupRef := range sys.Spec.GroupRefs {
		var sg uyuniv1.SystemGroup
		if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: groupRef.Name}, &sg); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // group not found yet; no channels to inherit
			}
			return nil, "", err
		}
		for _, ccRef := range sg.Spec.ConfigChannelRefs {
			var cc uyuniv1.ConfigurationChannel
			if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ccRef.Name}, &cc); err != nil {
				if client.IgnoreNotFound(err) == nil {
					continue
				}
				return nil, "", err
			}
			if cc.Status.UyuniID == 0 {
				continue
			}
			if !seen[cc.Spec.ID] {
				labels = append(labels, cc.Spec.ID)
				seen[cc.Spec.ID] = true
			}
		}
	}

	return labels, "", nil
}

// resolveProfileLabel returns the Cobbler profile label to use for provisioning.
// If spec.autoinstall.profileRef is set, the label is read from the referenced
// AutoinstallProfile CR. If spec.autoinstall.profile is set, it is returned as-is.
func (r *SystemReconciler) resolveProfileLabel(ctx context.Context, sys *uyuniv1.System) (label, wait string, err error) {
	ai := sys.Spec.Autoinstall
	if ai == nil {
		return "", "", nil
	}
	if ai.Profile != "" {
		return ai.Profile, "", nil
	}
	if ai.ProfileRef != nil {
		var ap uyuniv1.AutoinstallProfile
		if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ai.ProfileRef.Name}, &ap); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return "", fmt.Sprintf("AutoinstallProfile %q not found", ai.ProfileRef.Name), nil
			}
			return "", "", err
		}
		if ap.Status.DistributionLabel == "" {
			return "", fmt.Sprintf("AutoinstallProfile %q not yet realized (no distributionLabel in status)", ai.ProfileRef.Name), nil
		}
		return ap.Spec.Label, "", nil
	}
	return "", "", nil
}

// resolveGroupMembership returns the list of Uyuni group names the system should belong to.
func (r *SystemReconciler) resolveGroupMembership(ctx context.Context, sys *uyuniv1.System) (names []string, wait string, err error) {
	for _, ref := range sys.Spec.GroupRefs {
		var sg uyuniv1.SystemGroup
		if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ref.Name}, &sg); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("SystemGroup %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if sg.Status.UyuniID == 0 {
			return nil, fmt.Sprintf("SystemGroup %q not yet realized", ref.Name), nil
		}
		names = append(names, sg.Spec.Name)
	}
	return names, "", nil
}

func (r *SystemReconciler) fail(ctx context.Context, sys *uyuniv1.System, reason string, err error) (ctrl.Result, error) {
	setReady(&sys.Status.Conditions, sys.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, sys)
	return ctrl.Result{}, err
}

func (r *SystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.System{}).
		Watches(&uyuniv1.SystemGroup{},
			handler.EnqueueRequestsFromMapFunc(r.systemsForGroup)).
		Watches(&uyuniv1.ConfigurationChannel{},
			handler.EnqueueRequestsFromMapFunc(r.systemsForConfigChannel)).
		Watches(&uyuniv1.SoftwareChannel{},
			handler.EnqueueRequestsFromMapFunc(r.systemsForSoftwareChannel)).
		Watches(&uyuniv1.AutoinstallProfile{},
			handler.EnqueueRequestsFromMapFunc(r.systemsForAutoinstallProfile)).
		Complete(r)
}

func (r *SystemReconciler) systemsForGroup(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SystemList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sys := range list.Items {
		for _, ref := range sys.Spec.GroupRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: sys.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *SystemReconciler) systemsForConfigChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SystemList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sys := range list.Items {
		for _, ref := range sys.Spec.ConfigChannelRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: sys.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *SystemReconciler) systemsForSoftwareChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SystemList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sys := range list.Items {
		if sys.Spec.BaseChannelRef != nil && sys.Spec.BaseChannelRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: sys.Name},
			})
			continue
		}
		for _, ref := range sys.Spec.ChildChannelRefs {
			if ref.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: sys.Name},
				})
				break
			}
		}
	}
	return out
}

func (r *SystemReconciler) systemsForAutoinstallProfile(ctx context.Context, obj client.Object) []reconcile.Request {
	var list uyuniv1.SystemList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, sys := range list.Items {
		if sys.Spec.Autoinstall != nil && sys.Spec.Autoinstall.ProfileRef != nil &&
			sys.Spec.Autoinstall.ProfileRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: sys.Namespace, Name: sys.Name},
			})
		}
	}
	return out
}

