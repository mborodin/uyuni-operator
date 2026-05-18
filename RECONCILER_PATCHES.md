# Reconciler cleanup patches

Patches to apply to the reconciler files produced over the course of the
conversation. Each section shows the SEARCH / REPLACE pair plus the
rationale, in dependency order.

These patches assume the new files in this repo are in place:
- `internal/controller/finalizers.go` (ensureFinalizer, containsFinalizer, removeFinalizer)
- `internal/controller/annotations.go` (migrateAnnotations)
- `internal/controller/conditions.go` (setReady, setCondition, setDrift, condUyuniDrift)
- `internal/controller/constants.go` (finalizer constants)
- `internal/controller/channelresolve.go` (post-cleanup version)
- `internal/validation/*` (extracted pure-logic helpers)

================================================================================
## activationkey_controller.go
================================================================================

### 1) Add annotation migration at the top of Reconcile

After the initial Get, before any other work:

```go
if migrateAnnotations(&ak) {
    if err := r.Update(ctx, &ak); err != nil {
        return ctrl.Result{}, err
    }
    return ctrl.Result{}, nil
}
```

### 2) Replace controllerutil finalizer calls

```diff
- if controllerutil.AddFinalizer(&ak, finalizer) {
+ if ensureFinalizer(&ak, akFinalizer) {
      return ctrl.Result{Requeue: true}, r.Update(ctx, &ak)
  }

- if controllerutil.ContainsFinalizer(&ak, finalizer) {
+ if containsFinalizer(&ak, akFinalizer) {
      ...
-     controllerutil.RemoveFinalizer(&ak, finalizer)
+     removeFinalizer(&ak, akFinalizer)
      return ctrl.Result{}, r.Update(ctx, &ak)
  }
```

### 3) Force-delete escape hatch (in deletion path)

```go
if !ak.DeletionTimestamp.IsZero() {
    if !containsFinalizer(&ak, akFinalizer) {
        return ctrl.Result{}, nil
    }
    if ak.Annotations[uyuniv1.AnnForceDelete] == "true" {
        removeFinalizer(&ak, akFinalizer)
        return ctrl.Result{}, r.Update(ctx, &ak)
    }
    // ... existing Uyuni-side cleanup ...
}
```

### 4) Rename condition reason

```diff
- r.setReady(&ak, metav1.ConditionFalse, "InvalidChannelReference", res.HardError)
+ setReady(&ak.Status.Conditions, ak.Generation, metav1.ConditionFalse, "ReferenceUnavailable", res.HardError)
  return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, &ak)
```

The 30-second requeue is the new behavior for the (transient) "referenced
resource is currently broken" case. Hard errors of the
"admission-should-have-rejected" variety also fall here — they don't
typically self-heal but the customer should investigate quickly.

### 5) Project ownership reconciliation

After resolveDesired returns success (WaitReason and HardError both empty),
before any Uyuni API call:

```go
if err := reconcileProjectOwnership(ctx, r.Client, &ak, projectOwnersFromActivationKey(&ak)); err != nil {
    return ctrl.Result{}, err
}
```

### 6) Watch ContentProject for build-state changes

In SetupWithManager:

```go
return ctrl.NewControllerManagedBy(mgr).
    For(&uyuniv1.ActivationKey{}).
    Watches(&uyuniv1.ContentProject{}, handler.EnqueueRequestsFromMapFunc(r.activationKeysForProject)).
    Watches(&uyuniv1.SoftwareChannel{}, handler.EnqueueRequestsFromMapFunc(r.activationKeysForChannel)).
    Complete(r)
```

The mapper functions iterate ActivationKey lists in the project's namespace
and return reconcile requests for any that match via refsActivationKeyProject.

================================================================================
## system_controller.go
================================================================================

### Same pattern as ActivationKey

- Add migrateAnnotations at top of Reconcile.
- Replace controllerutil.{Add,Remove,Contains}Finalizer with the new helpers.
- Add force-delete annotation handling in deletion path (respecting
  DeletionPolicy=Orphan vs Delete).
- Rename InvalidChannelReference → ReferenceUnavailable for resolver-based errors.
- Add reconcileProjectOwnership call before scheduling channel changes.
- Drop the `hostname` defaulting fallback (webhook handles it). Keep a
  comment noting that the SystemDefaulter webhook is now responsible.

### Remove preCreate validation block

The "preCreate requires identification" check is gone (webhook handles).
The reconciler's call to uc.CreateSystemProfile will fail cleanly with a
Uyuni-side error if somehow admission was bypassed.

================================================================================
## contentproject_controller.go
================================================================================

### 1) Remove validateEnvChain call entirely

```diff
- if err := validateEnvChain(cp.Spec.Environments); err != nil {
-     return r.fail(ctx, &cp, "InvalidEnvironmentChain", err)
- }
```

The function itself is also deleted from this file (it was moved to
internal/validation/envchain.go).

### 2) Use validation.ChainOrder

```diff
- ordered := chainOrder(cp.Spec.Environments)
+ ordered := validation.ChainOrder(cp.Spec.Environments)
```

The private chainOrder function in this file goes away.

### 3) Cascading delete via owner refs (not findDependents)

Replace the entire handleDeletion implementation with the version from the
conversation: check AnnForceDelete first, then active promotions, then
pendingOwnedDependents (count CRs owned via UID), then Uyuni-side cleanup.

### 4) Finalizer migration

Standard: ensureFinalizer / containsFinalizer / removeFinalizer with cpFinalizer.

================================================================================
## contentprojectpromotion_controller.go
================================================================================

### Remove validatePromotionPair call

```diff
- if err := validatePromotionPair(&cp, p.Spec.FromEnvironment, p.Spec.ToEnvironment); err != nil {
-     return r.failPromotion(ctx, &p, err)
- }
```

Replace with a narrower runtime-state check:

```go
sourceState := findEnvState(cp.Status.EnvironmentStates, p.Spec.FromEnvironment)
if sourceState == nil {
    return r.failPromotion(ctx, &p, fmt.Errorf(
        "environment %q no longer exists in ContentProject %q (project was modified after promotion was created)",
        p.Spec.FromEnvironment, cp.Name))
}
```

================================================================================
## task_controller.go
================================================================================

### Remove kindCount check; rely on scheduleByKind default branch

```diff
- if kindCount(&task.Spec) != 1 {
-     return r.failTask(ctx, &task, "InvalidSpec",
-         fmt.Errorf("exactly one of highstate, remoteCommand, reboot, applyPatches, applyConfigChannels required"))
- }
```

The kindCount function itself moves to internal/validation (already done
in validation/task.go as TaskKindCount).

### Add migrateAnnotations at top, finalizer migration, force-delete handling

================================================================================
## uyuniprovider_controller.go
================================================================================

### Reword duplicate-default check as backstop

Extract to method, update message, comment:

```go
// Webhook rejects duplicate defaults at admission. Backstop here covers
// the race window of concurrent CR creates by different clients.
if prov.Spec.IsDefault {
    if dup, err := r.findOtherDefault(ctx, &prov); err != nil {
        return ctrl.Result{}, err
    } else if dup != "" {
        setReady(&prov.Status.Conditions, prov.Generation, metav1.ConditionFalse,
            "DuplicateDefault",
            fmt.Sprintf("UyuniProvider %q is also marked default; resolve by removing isDefault on one", dup))
        return ctrl.Result{}, r.Status().Update(ctx, &prov)
    }
}
```

================================================================================
## softwarechannel_controller.go
================================================================================

### Replace ImmutableFieldDrift error path with UyuniDrift condition

```diff
- if parentLabel != "" && current.ParentChannelLabel != parentLabel {
-     return r.fail(ctx, &ch, "ImmutableFieldDrift",
-         fmt.Errorf("parent channel changed in spec but Uyuni does not allow reparenting; delete and recreate"))
- }
+ drifted := parentLabel != "" && current.ParentChannelLabel != parentLabel
+ if drifted {
+     setDrift(&ch.Status.Conditions, ch.Generation, true, "ImmutableFieldDrift",
+         fmt.Sprintf("parent channel in Uyuni (%q) differs from spec (%q); WebUI or external tooling may have changed it",
+             current.ParentChannelLabel, parentLabel))
+ } else {
+     setDrift(&ch.Status.Conditions, ch.Generation, false, "InSync", "")
+ }
```

Webhook now rejects spec.parentChannelRef changes at admission. The
remaining code path catches Uyuni-side drift, which is informational.

Same pattern in repository_controller.go (type drift), configchannel_controller.go
(type drift). imageprofile_controller.go (type drift, label drift).
