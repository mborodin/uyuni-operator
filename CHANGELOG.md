# Changelog

## Unreleased

### Added

- **`Proxy` resource** — declarative management of a Uyuni containerized proxy's
  configuration. The operator calls `proxy.containerConfig` on the referenced
  provider, extracts the returned config archive (`config.yaml`, `httpd.yaml`,
  `ssh.yaml`, certs/keys) into an **operator-owned Secret** (named in
  `status.secretName`, garbage-collected with the CR), and surfaces the
  non-sensitive config (`config.yaml`) plus metadata (`status.files`,
  `inputHash`, `generatedAt`) in status — private keys never touch status.
  `spec.tlsSecretRef` supplies the proxy certificate from a `kubernetes.io/tls`
  Secret (caller-cert generation); omit it to have Uyuni reuse its own
  certificate. Because `proxy.containerConfig` rotates the proxy↔server SSH
  keypair on every call, regeneration is gated on a hash of the resolved inputs
  and the one-shot `uyuni.uyuni-project.org/regenerate` annotation, not on every
  reconcile. `spec.fqdn` is immutable (webhook-enforced). This adds the
  operator's first Secret **write** RBAC (`secrets: create/update/patch/delete`).
  The `proxy.containerConfig` call is POST with camelCase params, and its byte[]
  result is returned as a JSON array of *signed* integers (not base64) — both
  verified against a live Uyuni 2026.06 server.

### Fixed

- **Removed stray debug output from the System validator.** A leftover
  `fmt.Printf` in `internal/validation` `SystemFormulas` logged to the webhook's
  stdout on every empty-path `valuesFrom`; removed it (and the now-unused import).

- **The Cobbler system record now carries the system hostname.** A pre-created
  System's `spec.hostname` was only used for the Cobbler record *name*; the record
  itself had an empty `hostname` and no interface `dns_name`, so Cobbler couldn't
  resolve it. `reconcileCobblerSystem` now sets the Cobbler system `hostname` and
  marks the first NIC as the management interface with its `dns_name` = the
  hostname. Added `CobblerSystem.spec.hostname` and `interfaces[].management`.
- **Assigning formulas no longer 400s with "No method exists".**
  `formula.setFormulasOfServer` takes `systemId`, not `sid` (the docs say `sid`,
  but the live API rejects it — same doc inconsistency as `getSystemFormulaData`).
  Note the sibling `getFormulasByServerId` genuinely does use `sid`. Verified live.
- **Formulas are now applied to pre-created/bootstrap systems.** The reconcile's
  "still bootstrap-entitled" gate returned before `reconcileFormulas`, so a
  pre-created System (e.g. a retail/saltboot branch server) never got its formula
  data until it fully registered — too late, since saltboot needs the data (boot
  image URL, store config) to PXE-boot. Formula data is set via
  `setSystemFormulaData` (pillar data, not a scheduled action needing a live
  minion), so it is now pushed in the pre-registration path alongside group
  membership; software channels and add-ons still wait for registration.
- **Formula data reads no longer 403.** `formula.getSystemFormulaData` was called
  via POST with a `sid` param; it is a read method (POST → HTTP 403) and its param
  is `systemId`, not `sid` (`getFormulasByServerId` uses `sid` — hence the earlier
  confusion). It now does `GET …?systemId=<id>&formulaName=<name>`, which restores
  formula drift detection (`FormulaValuesDrift`). Verified against a live server.
- **Formula `objectFieldRef.fieldPath` no longer fails with "unrecognized
  identifier".** A downward-API-style path like `status.imageUrl` was wrapped as
  `{status.imageUrl}`, but k8s jsonpath needs a leading dot (`{.status.imageUrl}`)
  or it treats the first segment as an identifier. The evaluator now adds the
  leading dot, so `fieldPath: status.imageUrl` works.
- **ImageBuild no longer schedules a build twice in Uyuni.** A stale informer
  cache let a second reconcile see `status.actionId == 0` (before the first
  reconcile's status write had propagated) and call `image.scheduleImageBuild`
  again — leaving an orphaned duplicate build. The reconciler now re-reads the
  build straight from the API server (uncached) before scheduling and adopts an
  already-recorded action id instead of scheduling again.
- **`ImageProfile.spec.kiwiOptions` is now applied to existing profiles.** Uyuni
  accepts `kiwiOptions` only at `image.profile.create` (its `setDetails` has no
  such member and `getDetails` never returns it), so setting/adding it on a
  profile that already existed in Uyuni was silently ignored — `create` was never
  called. The reconciler now recreates the profile when `spec.kiwiOptions` differs
  from `status.appliedKiwiOptions`, applying the change exactly once. (Recreating
  is safe: built images and `ImageBuild` CRs reference the profile by label, which
  is preserved.)
- **`SoftwareChannel.status.packageCount` was always zero.**
  `channel/software/getDetails` has no `package_count`/`last_synced` fields —
  they were never real API keys, so the wire struct silently decoded them as
  zero every time, regardless of actual sync state. Package count now comes
  from `channel/software/listAllPackages` (`GetChannelPackageCount`). A new
  `PackagesSynced` condition (separate from `Ready`, since a channel can be
  fully reconciled with a broken repo URL) reports `NoRepositories` /
  `SyncInProgress` / `Synced` / `NoPackages` so a bad repository URL surfaces
  instead of silently leaving the channel empty.
- **`Repository` reported `Ready` for unreachable URLs.** Nothing validated
  `spec.url` was actually reachable — Uyuni accepts the repo config
  unconditionally at creation and only fails much later, async, during the
  channel's repo sync. `Repository` now does an http(s) reachability probe
  (`URLUnreachable` reason) before setting `Ready`; `file://`/`uln://`/`ftp://`
  aren't checked (unverifiable from the operator's pod) and always pass.
- **Manager crash-looped on startup under a slow/throttled API server.** Two
  independent timeouts were both too tight for a cluster with multi-second
  API request latency and 22 informers starting concurrently:
  - Default leader-election timings (`LeaseDuration: 15s`, `RenewDeadline:
    10s`) left no margin for a missed lease renewal, which made
    controller-runtime conclude it lost leadership and cancel the manager
    context mid-startup. `LeaseDuration`/`RenewDeadline`/`RetryPeriod` are now
    120s/90s/15s. (Deployments running a single replica should instead set
    `leaderElect: false` in the Helm chart values — leader election exists to
    coordinate multiple replicas and is pure overhead, and pure risk, with
    only one.)
  - controller-runtime's per-controller `CacheSyncTimeout` (default 2m) is
    **independent of leader election** and was the deeper cause: each
    controller's own `WaitForCacheSync` raced this 2m deadline regardless of
    leadership state, so the crash persisted even with leader election
    disabled entirely. Both surfaced identically: `failed to wait for X
    caches to sync` for whichever controller was still syncing when the
    deadline hit. `CacheSyncTimeout` is now 10m.
- **ActivationKey no longer fails with "Invalid channel" re-adding existing
  channels.** `activationkey.getDetails` is serialized in camelCase by the
  HTTP/JSON API (`childChannelLabels`, `baseChannelLabel`, …), but the wire
  struct used the documented snake_case tags, so the operator parsed empty
  base/child channels — then tried to add child channels that were already
  present, which Uyuni rejects as "Invalid channel" (children need a base
  channel and were already attached). The wire struct now matches the actual
  camelCase response, so current channels are read correctly and the base is set
  via `setDetails` before children are diffed.
- **System channel drift is now read correctly.** `system.getDetails` returns no
  channel subscriptions, so the reconciler's `current.baseChannelLabel` /
  `childChannelLabels` were always empty — it re-issued `setBaseChannel` /
  `setChildChannels` on every reconcile and the `scheduleChangeChannels` path was
  dead. The current channels are now read via `system.getSubscribedBaseChannel` +
  `system.listSubscribedChildChannels`, so channels are only changed on real
  drift. (Audited all `getDetails` wire structs against live responses while here;
  `Organization`, `SoftwareChannel`, `ConfigurationChannel`, and `SystemGroup`
  parse correctly.)
- **System identity no longer shows false `minionId` drift.** `system.getDetails`
  returns `minion_id` (snake_case), but the wire struct read `minionid`, so
  `current.MinionID` was always empty and every registered System with a
  `spec.minionID` was flagged as drifted (`minionId in Uyuni () differs from
  spec …`). Fixed the tag (and `name` → `profile_name`).
- **ImageProfile no longer calls `setDetails` on every reconcile.**
  `image.profile.getDetails` doesn't return the source `path`, so comparing it
  always reported drift. Path changes are now re-applied on a spec-generation
  change instead, and the readable fields (storeLabel/activationKey) still drive
  drift detection.
- **Action-status reads use GET, not POST (fixes the schedule `403`).** The Uyuni
  HTTP API routes `@ReadOnly` methods to GET only; POSTing `schedule.list*Systems`
  returns HTTP 403 (a web 403 page, *not* a permission problem — reproduced with a
  full admin user). `GetActionDetails`/`GetActionResults` now call
  `schedule.list{Failed,InProgress,Completed}Systems` via GET with the integer
  `actionId` as a query parameter. This also unblocks `Task` run status and
  `System` autoinstall status, which use the same calls.
- **ImageBuild detects failures via the build action, and records real image
  info.** Detection now polls the build *action* (`GetActionDetails`, GET), which
  reports `Failed`/`Completed` even when the build fails before any image record
  exists (e.g. the build host minion is down) — the case that previously left the
  build stuck. It no longer matches images by the tag we pass: for kiwi, the built
  image's `version` is the kiwi version (e.g. `0.1.3`), not that tag, and
  in-progress/failed builds appear only as `"Building profile: …"` with no
  version. Instead the build's own image record is identified by capturing the
  highest image id at schedule time (`status.baselineImageId`) and taking the
  first one created above it. On success it records the image `name`, `version`
  (the real kiwi version), `revision`, and uploaded `files`, surfaced as
  `Image`/`Version`/`Rev` print columns. The status/condition mismatch is gone
  (`Scheduled` before an image record appears, `Running` after), and
  `spec.timeoutMinutes` (default 120) remains only as a backstop for a build the
  action never reports as finished. Cancel-on-delete is best-effort.
- **ImageBuild no longer waits forever with `ImageProfile … not yet realized in
  Uyuni`.** The gate checked `ImageProfile.status.uyuniId`, but image profiles
  have no numeric id in the Uyuni API (`image.profile.getDetails` returns none),
  so it was always 0. ImageBuild now gates on the ImageProfile's `Ready`
  condition instead, which turns True once the operator has ensured the profile
  exists in Uyuni.
- **Registered systems no longer wedge with `unmarshal number into ... []int`.**
  Every `system.schedule*` call (`scheduleApplyHighstate`, `scheduleScriptRun`,
  `scheduleReboot`, `scheduleApplyErrata`, `scheduleApplyConfigChannel`) returns a
  single bare int action id, but several were decoding into `[]int`. In
  particular `ScheduleHighstate` runs on every reconcile of a System with
  `spec.applyHighState: true` and its failure kept `status.phase` from ever
  reaching `Reconciled`, so those systems retried forever. All schedule calls now
  decode `int`; `ScheduleReboot`/`ScheduleApplyPatches` return `(int, error)` to
  match. `changeProxy` remains an int array (the one method that truly returns
  one).
- **Image build / action polling no longer 403s.** `GetActionDetails` called
  `schedule.getScheduledActionDetails`, which does not exist in the Uyuni API —
  the request fell through to a web path returning an HTML/403 page, so every
  image build (and Task) poll failed after scheduling. Action status is now
  derived from `schedule.list{Failed,InProgress,Completed}Systems`, and the
  latent same-class bugs in `GetActionResults` (invented `status`/`result`/
  `exit_code` fields) and `CancelAction` (`action_ids` → `actionIds`) are fixed.
  All three now POST the integer `actionId` in the request body (int params in a
  GET query string yield "No method exists").
- **Scheduling an image build no longer 400s** (`No method exists with the
  matching parameters`). `image.scheduleImageBuild` now sends camelCase params
  (`profileLabel`, `version`, `buildHostId`) plus the required
  `earliestOccurrence`, and decodes the bare int action id it returns.
- **`baseChannelFrom`/`childChannelsFrom` with an empty `contentProjectRef` now
  attach the channel directly.** Previously such refs were dropped, so an
  ActivationKey (or System) that used `baseChannelFrom.sourceChannelLabel`
  without a content project ended up with no channels. An empty
  `contentProjectRef` now means "`sourceChannelLabel` is an existing Uyuni
  channel label, use it directly" (a bare `{name: ""}` with no
  `sourceChannelLabel` still means "no channel").

### Changed

- **ImageProfile-triggered builds now materialize an owned `ImageBuild` CR**
  (CronJob→Job pattern). A `buildPolicy: onChange` change or the
  `uyuni.uyuni-project.org/build-now` annotation used to schedule the build
  directly and track it only in `status.lastBuild`; it now creates a first-class
  `ImageBuild` (owned by the profile, GC'd with it) that does the scheduling,
  polling and artifact recording. The name is deterministic — `<profile>-gen<N>`
  for onChange, `<profile>-<version>` for build-now — so a trigger yields exactly
  one build and reruns are no-ops. `ImageProfile.status.lastBuild` now mirrors the
  newest referencing `ImageBuild`, and `status.lastBuildName` points at it. The
  saltboot `status.bootImage` is still read from the image pillar on success.

### Added

- **Structured formula config from ConfigMaps/Secrets.**
  `System.spec.formulas[].valuesFrom[]` entries now take an optional
  `format: string|yaml|json` (default `string`). With `yaml`/`json`, a
  Secret/ConfigMap key's value is deserialized into structured form data (nested
  maps/arrays) and placed at `path` — or, with an empty `path`, its top-level keys
  are merged at the form-data root. This lets a whole structured formula config
  live in one ConfigMap/Secret key. `format` is only valid with
  `secretKeyRef`/`configMapKeyRef` (`objectFieldRef` values are already typed).
  See `config/samples/formula-valuesfrom.yaml`.
- **`ImageStore` is now reconciled.** The previously dormant `ImageStore` type has
  a controller: it creates/updates/deletes the store in Uyuni (registry or OS
  image), reading registry credentials from `spec.credentialsSecretRef`
  (`username`/`password` keys). Fixed the client along the way — the API lives
  under `image.store` (path `image/store/*`, not `imagestore/*`, which 403s),
  `getDetails` returns `storetype` (not `store_type`) and no numeric id, and
  `create`/`setDetails` take a credentials/details struct. `ImageProfile` now
  gates on the store's `Ready` condition (image stores have no id) so a
  dockerfile profile no longer waits forever for its store to "realize".
- **`ImageProfile.spec.kiwiOptions`** — a free-form string of extra kiwi build
  options for `type: kiwi` OS images (e.g. `--profile <name>` to pick a profile
  from a multi-profile kiwi description). Passed to `image.profile.create`. Uyuni
  exposes no way to update it on an existing profile (`setDetails` has no such
  member), so it is immutable — the webhook rejects changes and a CEL rule
  restricts it to `type: kiwi`; recreate the ImageProfile to change it.
- **Formula config values from other resources.**
  `System.spec.formulas[].valuesFrom` sets specific paths in a formula's form
  data from a `secretKeyRef`, `configMapKeyRef`, or `objectFieldRef` (a JSONPath
  field of another `uyuni.uyuni-project.org` resource, same namespace only — the
  operator never reads arbitrary cluster kinds). `valuesFromSources` bulk-merges
  every key of a Secret/ConfigMap under a path (envFrom-style). **Reference
  values are applied only on a spec change or the
  `uyuni.uyuni-project.org/apply-formula-values` annotation** — a value that
  changes on its own (e.g. a new image build) surfaces as a `FormulaValuesDrift`
  condition instead of silently rewriting the system's config. See
  `config/samples/formula-valuesfrom.yaml`.
- **`ImageProfile.status.lastBuild` build artifacts.** A successful build now
  records `files[]` (`{name, type: image|kernel|initrd, url}`), `imageUrl` (the
  installable image's download URL), `checksum`, and `revision` — from
  `image.getDetails` — so formulas can reference e.g.
  `status.lastBuild.files[?(@.type=='image')].url`.
- **`ImageBuild` controller registered + build artifacts.** The previously
  dormant `ImageBuild` reconciler is now wired in (with its CRD in the
  kustomization + chart). Each `ImageBuild` is an immutable record of one build
  and records its own `status.files[]`/`imageUrl`/`checksum` on success — so you
  can pin a specific version's artifacts by referencing that `ImageBuild` rather
  than the ImageProfile's mutable `lastBuild`.

- **Cobbler controller + CRDs (`CobblerDistro`, `CobblerProfile`,
  `CobblerSystem`).** The operator now talks to Cobbler's XMLRPC API directly
  (via Uyuni's `/cobbler_api`), reusing the `UyuniProvider` credentials, instead
  of the fragile WebUI/JSON-kickstart workarounds. Each resource has a
  `mode: create|import` (default `import`): `import` observes an object created
  elsewhere (e.g. a Uyuni image build's distro/profile) and becomes Ready once it
  appears; `create` manages it. A `System` with `spec.autoinstall` now spawns an
  **owned `CobblerSystem`** (create mode) that writes the Cobbler record — named
  interfaces from `spec.network`, profile binding, netboot, and ks_meta
  (`autoinstallMeta`) from `spec.autoinstall.variables` — with the boot proxy
  from `spec.proxyRef`; the record name is stored in `System.status.
  cobblerSystemName`. Reads need no auth; writes require cobbler's
  `redhat_management_permissive` setting enabled (then the provider user's
  `config_admin`/`org_admin` role authenticates).

### Removed

- The WebUI/JSON autoinstall workarounds superseded by the Cobbler controller:
  `system.createSystemRecord`, the `Variables.do`/`ScheduleWizard.do` WebUI
  drivers (`SetVariables`, `GeneratePxeConfig`), and the System reconciler's
  `ensureAutoinstallRecord`/`AutoinstallRecordLabel`.

- **System: Cobbler-only profile support (image/PXE profiles).** Autoinstall
  profiles that have no Uyuni KickstartData (e.g. auto-created image/PXE
  profiles) are rejected by the JSON kickstart API (403 / "Invalid
  autoinstallation label") even for admins. The operator now drives the same
  WebUI actions the browser uses, via its existing session: `SetVariables` posts
  to `Variables.do` (ks_meta + netboot), and a new `GeneratePxeConfig` posts the
  ScheduleWizard "generate PXE installation configuration" action to create the
  Cobbler system record and bind the profile, passing the boot proxy (resolved
  from `spec.proxyRef` to its Uyuni server id). Applied for pre-created systems
  with `spec.autoinstall`.

- **System: Cobbler system record on pre-create.** When a pre-created
  (`preCreate: true`) System has `spec.autoinstall` set, the reconciler now
  follows the UI flow — after `system.createSystemProfile` it calls
  `system.createSystemRecord` with the interface list from `spec.network`, so
  Uyuni shows real interface names (`eth0`, ...) instead of `undefined` and the
  Cobbler record is linked to the autoinstall profile for PXE boot. The resolved
  profile label is tracked in `status.autoinstallRecordLabel` for idempotency.
- **System: autoinstall variables (env-style) + netboot toggle.**
  `System.spec.autoinstall.variables` sets per-system Cobbler system-record
  variables (ks_meta) via `system.setVariables`. Each entry is shaped like a pod
  env var — a literal `value` or a `valueFrom` sourcing a `secretKeyRef` /
  `configMapKeyRef` (same namespace). `spec.autoinstall.variablesFrom` bulk-imports
  every key of a Secret/ConfigMap (like `envFrom`, with optional `prefix`);
  explicit `variables` override imported keys. `spec.autoinstall.netboot` (default
  `true`) toggles PXE netboot on the Cobbler record, matching the UI. Applied only
  when `preCreate` is true. **`system.setVariables` REPLACES the record's entire
  ks_meta**, so the declared set is authoritative and complete — the operator only
  calls it when at least one variable is declared (never wiping Uyuni-generated
  metadata by accident). Requires the operator's new `configmaps` get/list/watch
  RBAC.

- **External autoinstallation profiles.** `AutoinstallProfile.spec.mode:
  Managed|External` (default `Managed`). In `External` mode the operator
  observes an existing Cobbler-managed profile (e.g. one Uyuni auto-creates for
  a PXE/OS image) — it never creates, mutates, or deletes it — and publishes the
  observed tree label to `status.distributionLabel` so `System.spec.autoinstall.
  profileRef` can boot from it. `distributionRef`/`rootPasswordSecretRef` become
  optional (forbidden in External mode, required in Managed, enforced by CEL).
  The `AutoinstallProfile` reconciler + validating webhook are now **registered**.
- **ImageProfile: saltboot boot image.** After a successful PXE/OS-image build,
  `ImageProfile.status.bootImage` is populated from the image pillar with the
  saltboot boot image identifier (e.g. `BranchServer_MicroOS-0.6.10-4`) — wire it
  into a saltboot formula (`System.spec.formulas`) to PXE-boot systems with that
  image. The `ImageProfile` reconciler is now **registered**. (Uyuni's PXE/OS
  images boot via saltboot, not classic Cobbler autoinstall profiles, which are
  not exposed through the kickstart API.)
- **System: Salt formulas.** `System.spec.formulas` enables Salt formulas on a
  system and supplies their form data — arbitrary nested config matching the
  formula's form (`formula.setFormulasOfServer` / `formula.setSystemFormulaData`).
- **System: proxy connection.** `System.spec.proxyRef` connects a system through
  another registered System acting as a Uyuni proxy (`system.changeProxy`).
  Clearing it reconnects the system directly to the server.
- **`CustomInfoKey` CR** manages organization-level custom system info key
  definitions (`system.custominfo`). Deletion is blocked while any System still
  references it (`Ready=False/InUse`).
- **`AutoinstallDistribution`** reconciler + validating webhook are now
  registered and its CRD is shipped (`kickstart.tree`).

### Breaking changes

**`System.spec.customInfo` (map[string]string) replaced by
`System.spec.customInfoValues`.** Each entry is `{ keyRef, value }`, where
`keyRef` references a `CustomInfoKey` CR — guaranteeing the key exists in Uyuni
before a value is set. Migrate every `customInfo: {k: v}` into a `CustomInfoKey`
CR plus `customInfoValues: [{ keyRef: {name: ...}, value: v }]`.

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
