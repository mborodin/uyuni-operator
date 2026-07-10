package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/cobbler"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// CobblerPool provides Cobbler XMLRPC clients resolved from a UyuniProvider,
// reusing the provider's URL, credentials and TLS. Implemented by pool.Pool.
type CobblerPool interface {
	Cobbler(ctx context.Context, ref *uyuni.LocalObjectRef, namespace string) (*cobbler.Client, error)
}

const (
	// cobbler heartbeat/requeue cadences.
	cobblerHeartbeat = 5 * time.Minute
	cobblerWait      = 30 * time.Second
)

func strVal(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func cobblerMode(m uyuniv1.CobblerMode) uyuniv1.CobblerMode {
	if m == "" {
		return uyuniv1.CobblerModeImport
	}
	return m
}

// cobblerProviderRefForOrg resolves the UyuniProvider backing an Organization CR
// (by name, in ns) so a spawned Cobbler* resource targets the same Cobbler.
// Nil = default provider.
func cobblerProviderRefForOrg(ctx context.Context, c client.Client, ns, orgName string) *uyuniv1.LocalObjectRef {
	if orgName == "" {
		return nil
	}
	var org uyuniv1.Organization
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: orgName}, &org); err != nil {
		return nil
	}
	if org.Spec.ProviderRef.Name == "" {
		return nil
	}
	return &uyuniv1.LocalObjectRef{Name: org.Spec.ProviderRef.Name}
}

// =============================================================================
// CobblerSystem
// =============================================================================

// CobblerSystemReconciler reconciles a CobblerSystem: import observes an
// existing record; create manages it via Cobbler XMLRPC.
type CobblerSystemReconciler struct {
	client.Client
	Cobbler CobblerPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblersystems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblersystems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblersystems/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerprofiles,verbs=get;list;watch

func (r *CobblerSystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cs uyuniv1.CobblerSystem
	if err := r.Get(ctx, req.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cc, err := r.Cobbler.Cobbler(ctx, toProviderRef(cs.Spec.ProviderRef), cs.Namespace)
	if err != nil {
		return r.fail(ctx, &cs, "ProviderError", err)
	}
	mode := cobblerMode(cs.Spec.Mode)

	if !cs.DeletionTimestamp.IsZero() {
		if containsFinalizer(&cs, cobSysFinalizer) {
			if mode == uyuniv1.CobblerModeCreate && cs.Annotations[uyuniv1.AnnForceDelete] != "true" {
				if err := cc.RemoveSystem(ctx, cs.Spec.Name); err != nil {
					return r.fail(ctx, &cs, "DeleteFailed", err)
				}
			}
			removeFinalizer(&cs, cobSysFinalizer)
			return ctrl.Result{}, r.Update(ctx, &cs)
		}
		return ctrl.Result{}, nil
	}
	if ensureFinalizer(&cs, cobSysFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cs)
	}

	if mode == uyuniv1.CobblerModeImport {
		item, found, err := cc.GetSystem(ctx, cs.Spec.Name)
		if err != nil {
			return r.fail(ctx, &cs, "CobblerError", err)
		}
		if !found {
			return r.waiting(ctx, &cs, fmt.Sprintf("cobbler system %q not found yet", cs.Spec.Name))
		}
		cs.Status.CobblerID = strVal(item, "uid")
		cs.Status.ProfileName = strVal(item, "profile")
		return r.ready(ctx, &cs, "Observed", "cobbler system observed")
	}

	// create mode
	profile, wait, err := r.resolveProfileName(ctx, &cs)
	if err != nil {
		return r.fail(ctx, &cs, "ResolveRefs", err)
	}
	if wait != "" {
		return r.waitReason(ctx, &cs, "WaitingForProfile", wait)
	}
	netboot := cs.Spec.NetbootEnabled == nil || *cs.Spec.NetbootEnabled
	ifaces := make([]cobbler.SystemInterface, 0, len(cs.Spec.Interfaces))
	for _, n := range cs.Spec.Interfaces {
		ifaces = append(ifaces, cobbler.SystemInterface{Name: n.Name, MAC: n.MACAddress, IP: n.IPAddress, DNSName: n.DNSName, Management: n.Management})
	}
	uid, err := cc.UpsertSystem(ctx, cobbler.SystemSpec{
		Name:            cs.Spec.Name,
		Hostname:        cs.Spec.Hostname,
		Profile:         profile,
		Netboot:         netboot,
		AutoinstallMeta: cs.Spec.AutoinstallMeta,
		Interfaces:      ifaces,
		Server:          cs.Spec.Server,
		Comment:         cs.Spec.Comment,
	})
	if err != nil {
		return r.fail(ctx, &cs, "CreateFailed", err)
	}
	cs.Status.CobblerID = uid
	cs.Status.ProfileName = profile
	return r.ready(ctx, &cs, "Reconciled", "cobbler system record reconciled")
}

func (r *CobblerSystemReconciler) resolveProfileName(ctx context.Context, cs *uyuniv1.CobblerSystem) (string, string, error) {
	if cs.Spec.ProfileName != "" {
		return cs.Spec.ProfileName, "", nil
	}
	if cs.Spec.ProfileRef != nil {
		var p uyuniv1.CobblerProfile
		if err := r.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Spec.ProfileRef.Name}, &p); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return "", fmt.Sprintf("CobblerProfile %q not found", cs.Spec.ProfileRef.Name), nil
			}
			return "", "", err
		}
		return p.Spec.Name, "", nil
	}
	return "", "", nil
}

func (r *CobblerSystemReconciler) fail(ctx context.Context, cs *uyuniv1.CobblerSystem, reason string, err error) (ctrl.Result, error) {
	setReady(&cs.Status.Conditions, cs.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, cs)
	return ctrl.Result{}, err
}

func (r *CobblerSystemReconciler) waiting(ctx context.Context, cs *uyuniv1.CobblerSystem, msg string) (ctrl.Result, error) {
	return r.waitReason(ctx, cs, "WaitingForCobblerObject", msg)
}

func (r *CobblerSystemReconciler) waitReason(ctx context.Context, cs *uyuniv1.CobblerSystem, reason, msg string) (ctrl.Result, error) {
	cs.Status.Found = false
	cs.Status.ObservedGeneration = cs.Generation
	setReady(&cs.Status.Conditions, cs.Generation, metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: cobblerWait}, nil
}

func (r *CobblerSystemReconciler) ready(ctx context.Context, cs *uyuniv1.CobblerSystem, reason, msg string) (ctrl.Result, error) {
	cs.Status.Found = true
	cs.Status.ObservedGeneration = cs.Generation
	setReady(&cs.Status.Conditions, cs.Generation, metav1.ConditionTrue, reason, msg)
	if err := r.Status().Update(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: cobblerHeartbeat}, nil
}

func (r *CobblerSystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).For(&uyuniv1.CobblerSystem{}).Complete(r)
}

// =============================================================================
// CobblerDistro
// =============================================================================

// CobblerDistroReconciler reconciles a CobblerDistro.
type CobblerDistroReconciler struct {
	client.Client
	Cobbler CobblerPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerdistros,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerdistros/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerdistros/finalizers,verbs=update

func (r *CobblerDistroReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cd uyuniv1.CobblerDistro
	if err := r.Get(ctx, req.NamespacedName, &cd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cc, err := r.Cobbler.Cobbler(ctx, toProviderRef(cd.Spec.ProviderRef), cd.Namespace)
	if err != nil {
		setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionFalse, "ProviderError", err.Error())
		_ = r.Status().Update(ctx, &cd)
		return ctrl.Result{}, err
	}
	mode := cobblerMode(cd.Spec.Mode)

	if !cd.DeletionTimestamp.IsZero() {
		if containsFinalizer(&cd, cobDistFinalizer) {
			if mode == uyuniv1.CobblerModeCreate && cd.Annotations[uyuniv1.AnnForceDelete] != "true" {
				if err := cc.RemoveDistro(ctx, cd.Spec.Name); err != nil {
					setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionFalse, "DeleteFailed", err.Error())
					_ = r.Status().Update(ctx, &cd)
					return ctrl.Result{}, err
				}
			}
			removeFinalizer(&cd, cobDistFinalizer)
			return ctrl.Result{}, r.Update(ctx, &cd)
		}
		return ctrl.Result{}, nil
	}
	if ensureFinalizer(&cd, cobDistFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cd)
	}

	if mode == uyuniv1.CobblerModeImport {
		item, found, err := cc.GetDistro(ctx, cd.Spec.Name)
		if err != nil {
			setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionFalse, "CobblerError", err.Error())
			_ = r.Status().Update(ctx, &cd)
			return ctrl.Result{}, err
		}
		if !found {
			cd.Status.Found = false
			cd.Status.ObservedGeneration = cd.Generation
			setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionFalse, "WaitingForCobblerObject",
				fmt.Sprintf("cobbler distro %q not found yet", cd.Spec.Name))
			_ = r.Status().Update(ctx, &cd)
			return ctrl.Result{RequeueAfter: cobblerWait}, nil
		}
		cd.Status.CobblerID = strVal(item, "uid")
		return r.done(ctx, &cd, "Observed", "cobbler distro observed")
	}

	uid, err := cc.UpsertDistro(ctx, cobbler.DistroSpec{
		Name:            cd.Spec.Name,
		Kernel:          cd.Spec.Kernel,
		Initrd:          cd.Spec.Initrd,
		Breed:           cd.Spec.Breed,
		OSVersion:       cd.Spec.OSVersion,
		Arch:            cd.Spec.Arch,
		KernelOptions:   cd.Spec.KernelOptions,
		AutoinstallMeta: cd.Spec.AutoinstallMeta,
	})
	if err != nil {
		setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionFalse, "CreateFailed", err.Error())
		_ = r.Status().Update(ctx, &cd)
		return ctrl.Result{}, err
	}
	cd.Status.CobblerID = uid
	return r.done(ctx, &cd, "Reconciled", "cobbler distro reconciled")
}

func (r *CobblerDistroReconciler) done(ctx context.Context, cd *uyuniv1.CobblerDistro, reason, msg string) (ctrl.Result, error) {
	cd.Status.Found = true
	cd.Status.ObservedGeneration = cd.Generation
	setReady(&cd.Status.Conditions, cd.Generation, metav1.ConditionTrue, reason, msg)
	if err := r.Status().Update(ctx, cd); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: cobblerHeartbeat}, nil
}

func (r *CobblerDistroReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).For(&uyuniv1.CobblerDistro{}).Complete(r)
}

// =============================================================================
// CobblerProfile
// =============================================================================

// CobblerProfileReconciler reconciles a CobblerProfile.
type CobblerProfileReconciler struct {
	client.Client
	Cobbler CobblerPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=cobblerdistros,verbs=get;list;watch

func (r *CobblerProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cp uyuniv1.CobblerProfile
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cc, err := r.Cobbler.Cobbler(ctx, toProviderRef(cp.Spec.ProviderRef), cp.Namespace)
	if err != nil {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "ProviderError", err.Error())
		_ = r.Status().Update(ctx, &cp)
		return ctrl.Result{}, err
	}
	mode := cobblerMode(cp.Spec.Mode)

	if !cp.DeletionTimestamp.IsZero() {
		if containsFinalizer(&cp, cobProfFinalizer) {
			if mode == uyuniv1.CobblerModeCreate && cp.Annotations[uyuniv1.AnnForceDelete] != "true" {
				if err := cc.RemoveProfile(ctx, cp.Spec.Name); err != nil {
					setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "DeleteFailed", err.Error())
					_ = r.Status().Update(ctx, &cp)
					return ctrl.Result{}, err
				}
			}
			removeFinalizer(&cp, cobProfFinalizer)
			return ctrl.Result{}, r.Update(ctx, &cp)
		}
		return ctrl.Result{}, nil
	}
	if ensureFinalizer(&cp, cobProfFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &cp)
	}

	if mode == uyuniv1.CobblerModeImport {
		item, found, err := cc.GetProfile(ctx, cp.Spec.Name)
		if err != nil {
			setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "CobblerError", err.Error())
			_ = r.Status().Update(ctx, &cp)
			return ctrl.Result{}, err
		}
		if !found {
			cp.Status.Found = false
			cp.Status.ObservedGeneration = cp.Generation
			setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "WaitingForCobblerObject",
				fmt.Sprintf("cobbler profile %q not found yet", cp.Spec.Name))
			_ = r.Status().Update(ctx, &cp)
			return ctrl.Result{RequeueAfter: cobblerWait}, nil
		}
		cp.Status.CobblerID = strVal(item, "uid")
		cp.Status.DistroName = strVal(item, "distro")
		return r.done(ctx, &cp, "Observed", "cobbler profile observed")
	}

	distro, wait, err := r.resolveDistroName(ctx, &cp)
	if err != nil {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "ResolveRefs", err.Error())
		_ = r.Status().Update(ctx, &cp)
		return ctrl.Result{}, err
	}
	if wait != "" {
		cp.Status.ObservedGeneration = cp.Generation
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "WaitingForDistro", wait)
		_ = r.Status().Update(ctx, &cp)
		return ctrl.Result{RequeueAfter: cobblerWait}, nil
	}
	uid, err := cc.UpsertProfile(ctx, cobbler.ProfileSpec{
		Name:            cp.Spec.Name,
		Distro:          distro,
		Autoinstall:     cp.Spec.Autoinstall,
		KernelOptions:   cp.Spec.KernelOptions,
		AutoinstallMeta: cp.Spec.AutoinstallMeta,
		EnableMenu:      cp.Spec.EnableMenu,
	})
	if err != nil {
		setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionFalse, "CreateFailed", err.Error())
		_ = r.Status().Update(ctx, &cp)
		return ctrl.Result{}, err
	}
	cp.Status.CobblerID = uid
	cp.Status.DistroName = distro
	return r.done(ctx, &cp, "Reconciled", "cobbler profile reconciled")
}

func (r *CobblerProfileReconciler) resolveDistroName(ctx context.Context, cp *uyuniv1.CobblerProfile) (string, string, error) {
	if cp.Spec.DistroName != "" {
		return cp.Spec.DistroName, "", nil
	}
	if cp.Spec.DistroRef != nil {
		var cd uyuniv1.CobblerDistro
		if err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Spec.DistroRef.Name}, &cd); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return "", fmt.Sprintf("CobblerDistro %q not found", cp.Spec.DistroRef.Name), nil
			}
			return "", "", err
		}
		return cd.Spec.Name, "", nil
	}
	return "", "", nil
}

func (r *CobblerProfileReconciler) done(ctx context.Context, cp *uyuniv1.CobblerProfile, reason, msg string) (ctrl.Result, error) {
	cp.Status.Found = true
	cp.Status.ObservedGeneration = cp.Generation
	setReady(&cp.Status.Conditions, cp.Generation, metav1.ConditionTrue, reason, msg)
	if err := r.Status().Update(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: cobblerHeartbeat}, nil
}

func (r *CobblerProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).For(&uyuniv1.CobblerProfile{}).Complete(r)
}
