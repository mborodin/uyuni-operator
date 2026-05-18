# Changelog

## Unreleased

### Breaking changes

**`providerRef` replaced by `organizationRef` on all namespace-scoped resources.**
All CRs that previously referenced a `UyuniProvider` directly now reference an
`Organization` CR instead. The new `Organization` CR owns the provider relationship
and, optionally, separate org-admin credentials.

#### Migration

1. Create an `Organization` CR in each namespace, pointing to the existing
   `UyuniProvider` via `spec.providerRef`. Use `spec.import.organizationId`
   to adopt the pre-existing Uyuni org rather than creating a new one.
2. On every `ActivationKey`, `System`, `SystemGroup`, `SoftwareChannel`,
   `Repository`, `ConfigChannel`, `ConfigFile`, `ContentProject`,
   `ContentProjectPromotion`, `ImageStore`, `ImageProfile`, and `Task`:
   replace `providerRef: {name: ...}` with `organizationRef: {name: ...}`.
   The referenced name is now the `Organization` CR name, not the
   `UyuniProvider` name.



**API group renamed from `uyuni.io` to `uyuni.uyuni-project.org`** to align
with the upstream Uyuni Operator (`cbosdo/uyuni-operator`) and the
broader `uyuni-project.org` ecosystem.

#### Migration

The operator handles most of the transition automatically, but you should
update your own assets:

* **CRs**: change `apiVersion: uyuni.io/v1alpha1` to
  `apiVersion: uyuni.uyuni-project.org/v1alpha1`. New CRs must use the new
  group; the old CRDs remain registered for a transition window so existing
  CRs continue to reconcile while you migrate.

* **Annotations**: `uyuni.io/force-delete`, `uyuni.io/rerun`,
  `uyuni.io/build-now`, `uyuni.io/sync-now`, and `uyuni.io/build-version`
  must be updated to the `uyuni.uyuni-project.org/*` equivalents before
  upgrading. The automatic migration shim has been removed.

* **Finalizers**: must be on `uyuni.uyuni-project.org/*` before upgrading.
  The compatibility shim that migrated `uyuni.io/*` finalizers on first
  reconcile has been removed. See **Removed** below for recovery steps.

* **RBAC**: any custom roles granting access to `uyuni.io` resources should
  also include `uyuni.uyuni-project.org`. Shipped roles are updated.

### Removed

Legacy `uyuni.io` compatibility shims have been deleted. The transition
window is over; the operator no longer recognises or migrates the old API
group at runtime.

* **Finalizers**: reconcilers no longer accept or migrate `uyuni.io/*`
  finalizers. Any CR still carrying a `uyuni.io/*` finalizer after upgrade
  will be stuck in terminating state. Remove the stale finalizer with
  `kubectl patch <kind> <name> -p '{"metadata":{"finalizers":[]}}' --type=merge`.

* **Annotations**: `uyuni.io/force-delete`, `uyuni.io/rerun`,
  `uyuni.io/build-now`, `uyuni.io/sync-now`, and `uyuni.io/build-version`
  are no longer recognised. Use the `uyuni.uyuni-project.org/*` equivalents.

* Internal: `legacyAliases` map, `legacyAnnotationMap` map,
  `migrateAnnotations` helper, and all per-reconciler migration call-sites
  have been deleted.

### Cleaned up

Validation that previously ran in reconcilers has moved to admission webhooks
where it can reject bad CRs at `kubectl apply` time instead of leaving them
in `Ready=False` state.

* `ActivationKey`, `System`: mutual exclusion of `*Ref` and `*From` fields,
  immutability of `spec.key`/`spec.minionId`, `preCreate` identification
  requirement, strict-`true` enforcement on dangerous annotations.
* `ContentProject`: environment chain structural validation (single root,
  no cycles, unique labels, predecessors declared), cron schedule syntax,
  unique filter names, `spec.label` immutability.
* `ContentProjectPromotion`: source/target validity against project chain,
  spec immutability past `Pending` phase.
* `Task`: discriminator validation (exactly-one-of kind, exactly-one-of
  target), `RemoteCommand` field bounds, spec immutability after first run.
* `UyuniProvider`: at-most-one-default-per-cluster.

Reconcilers retain narrow defense-in-depth checks for race conditions
(e.g., `UyuniProvider` duplicate-default at admission still doesn't help if
two providers are created concurrently). These now log "should have been
rejected at admission" diagnostics so an operator sees the webhook is
misconfigured.

### Added

* **`Organization` CRD** (namespace-scoped) representing a Uyuni organization.
  - `spec.providerRef` — required; the `UyuniProvider` used for satellite-admin
    operations (org create/delete).
  - `spec.credentialsSecretRef` — optional; org-admin credentials. When set,
    resource reconcilers connect to Uyuni as this user, scoping all operations
    to the org's namespace. Required when creating a new org (i.e., when
    `spec.import` is absent). The Secret must contain `username` and `password`
    keys; `firstName`, `lastName`, and `email` are optional (used only at
    org creation, with safe defaults if absent).
  - `spec.import.organizationId` — optional; links the CR to a pre-existing
    Uyuni org. The org is adopted (not created) and will not be deleted when
    the CR is removed.
  - Status: `uyuniOrgId`, `Ready` condition.
  - `uyuni.uyuni-project.org/force-delete` annotation skips Uyuni-side
    deletion when removing the CR.

* `UyuniDrift` condition on resources where Uyuni-side mutation is possible
  via the WebUI (`SoftwareChannel`, `Repository`, `ConfigChannel`). Surfaces
  out-of-band modification without blocking reconcile of mutable fields.
  Conditions printcolumn added: `kubectl get repository` now shows DRIFT.

* Validation package (`internal/validation`) with pure-function structural
  checks. Used by webhooks and (rarely) reconcilers; fast-running tests
  cover the validation surface exhaustively.

* Shared annotation/finalizer constants in `api/v1alpha1`. No more hardcoded
  strings spread across the codebase.

### Notes

* Conversion webhook between old and new group is not provided. The "both
  CRDs registered" approach is simpler and sufficient for v0.x. If you need
  to mass-migrate existing CRs to the new group, `kubectl get <kind> -A -o
  yaml | sed s,uyuni.io/v1alpha1,uyuni.uyuni-project.org/v1alpha1, |
  kubectl apply -f -` works.
