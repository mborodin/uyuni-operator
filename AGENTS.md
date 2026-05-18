# AGENTS.md

Operational guide for AI agents and humans contributing to this Kubernetes
operator. Read this before touching code. The goal is to keep the
operator's design principles intact as it grows.

If you're a human looking for build instructions, also check `README.md`.
If you're an agent picking up a task mid-stream, read this end-to-end.

---

## What this operator is

`uyuni-operator` is a Kubernetes operator that manages [Uyuni](https://www.uyuni-project.org/)
configuration and lifecycle from declarative CRs. It's a **separate
project** from the upstream `cbosdo/uyuni-operator`, which deploys
Uyuni itself; this one assumes a Uyuni server already exists and manages
its contents (channels, activation keys, systems, content projects, etc.).

The API group `uyuni.uyuni-project.org` is shared with the upstream
operator by convention but the CRDs are non-overlapping.

### User profile

The operator is built for a people running infrastructure on RKE2 with
Flux for GitOps. They want declarative, Git-tracked control of their Uyuni
fleet: channel sync, content lifecycle promotions, system pre-creation
for PXE boot, activation keys, image builds.

This shapes design tradeoffs throughout:
- GitOps-friendly: tolerant of out-of-order resource application (warnings
  not errors for "referenced thing not found yet")
- Audit-friendly: spec is intent, status records realized state, promotions
  are committed CRs (audit trail in git)
- Customer's ops team uses `kubectl` and Grafana — every meaningful state
  transition gets a condition with a clear reason

---

## Project layout

```
api/v1alpha1/                # CRD types, one file per resource family
  groupversion_info.go       # Group/Version declaration
  annotations.go             # Annotation key constants (uyuni.uyuni-project.org/*)
  common_types.go            # LocalObjectRef, ChannelFromProject
  *_types.go                 # One per resource family

internal/validation/         # Pure-function spec validators
  envchain.go                # EnvChain, ChainOrder, isAcyclic
  promotion.go               # PromotionPair
  task.go                    # TaskKindCount, TaskTargetCount, TaskSpec
  refs.go                    # ChannelRefMutex, PreCreateRequiresIdentification
  annotations.go             # StrictBooleanAnnotations
  validation_test.go         # Exhaustive table-driven tests, no I/O

internal/controller/         # Reconcilers + shared helpers
  constants.go               # Finalizer strings, legacy alias maps
  finalizers.go              # ensureFinalizer/containsFinalizer/removeFinalizer
  annotations.go             # migrateAnnotations (legacy uyuni.io/* promotion)
  conditions.go              # setReady/setCondition/setDrift + condition type names
  channelresolve.go          # resolveChannelRefs (direct + project-environment)
  ownership.go               # reconcileProjectOwnership for cascade-delete
  sets.go                    # diffStringSets, diffIntSets, diffCustomInfo
  <resource>_controller.go   # One reconciler per CR family

internal/webhook/            # Admission webhooks
  <resource>_webhook.go      # Validators (and defaulter for System)

internal/uyuni/              # Uyuni JSON API client
  api.go                     # API interface (impl + test fake both satisfy)
  types.go                   # Wire types (ChannelDetails, ProjectDetails, ...)
  uyunitest/                 # State-based FakeAPI for reconciler tests

config/                      # Kustomize manifests
  rbac/role.yaml             # Generated from kubebuilder markers
  samples/                   # Customer-facing example YAMLs
  webhook/manifests.yaml     # ValidatingWebhookConfiguration + cert-manager
```

---

## CRD inventory

| CR | Scope | Purpose |
|---|---|---|
| `UyuniProvider` | Cluster | Connection config to a Uyuni server (URL, creds secret ref) |
| `SoftwareChannel` | Namespaced | Uyuni software channel + sync schedule |
| `Repository` | Namespaced | yum/deb/uln repository, associated with channels |
| `ActivationKey` | Namespaced | Activation key with channels, groups, config channels |
| `System` | Namespaced | System lifecycle (pre-create, channels, add-ons) |
| `SystemGroup` | Namespaced | System group with declarative membership |
| `ConfigChannel` | Namespaced | Salt config/state/dictionary channel |
| `ConfigFile` | Namespaced | File/directory/symlink inside a ConfigChannel |
| `ImageStore` | Namespaced | Container registry or OS image store |
| `ImageProfile` | Namespaced | Kiwi or Dockerfile build profile |
| `ContentProject` | Namespaced | CLM project (sources, environments, filters) |
| `ContentProjectPromotion` | Namespaced | One-shot promotion action (Job-shaped, TTL'd) |
| `Task` | Namespaced | Scheduled action: highstate, command, reboot, patches, configs |

Implementation status (as of this writing):

- Types declared, validation in webhooks, reconciler logic post-cleanup:
  `UyuniProvider`, `SoftwareChannel`, `Repository`, `ActivationKey`,
  `System`, `SystemGroup`, `ConfigChannel`, `ConfigFile`, `ImageStore`,
  `ImageProfile`, `ContentProject`, `ContentProjectPromotion`, `Task`

- Reconcilers checked in: `ActivationKey`, `Repository`, `UyuniProvider`,
  `ContentProject`, `ContentProjectPromotion`, `Task`. The rest follow
  the same patterns documented below — `RECONCILER_PATCHES.md` has the
  cleanup intent for those that exist in draft form.

- API client (`*Client`): interface defined in `internal/uyuni/api.go`,
  HTTP implementation pending. Test fake `uyunitest.FakeAPI` directory
  scaffolded.

- `main.go` and `cmd/`: pending. Pattern: instantiate `ClientPool`, register
  each reconciler + webhook with the manager.

---

## Design principles

These are non-negotiable. New code that contradicts them needs a strong
argument in PR description.

### 1. Spec is intent; status is reality

CRs hold what the customer wants. Status reflects what we observed in
Uyuni. Never write derived state to spec. Never read intent from status.

Corollary: when computing drift, compare `spec.X` to the value read fresh
from Uyuni — never to `status.X` (that's a cache).

### 2. Reconciler convergence, not transactions

Every reconciler must be idempotent and eventually consistent. If you
catch yourself thinking "this only works if it's the first reconcile,"
rewrite. Standard patterns:

- Adoption via probe: lookup the external object by predictable name
  before assuming we need to create it. Handles operator restart mid-create.
- `RequeueAfter` heartbeat catches Uyuni-side drift even when no CR event fires.
- `ObservedGeneration` tracks "we have processed this generation"; it's not
  a guarantee that everything succeeded.

### 3. Webhooks validate; reconcilers converge

After the cleanup pass, this is the rule:

- Anything that's a pure spec problem (mutual exclusion, immutability,
  structural validation, kind discriminator cardinality) lives in webhooks.
- Anything that depends on runtime state (referenced thing not yet built,
  in-flight build, registration pending) lives in reconcilers.
- Reconciler retains *narrow* defense-in-depth for race windows and
  webhook-bypass scenarios. These checks must emit messages like
  `"admission should have rejected; check webhook configuration"` so
  the diagnostic points at the real problem.

### 4. Failure modes have clear names

Every `setReady(... metav1.ConditionFalse, reason, message)` call uses
a documented reason. Customer's alerting watches these strings; don't
rename without a CHANGELOG entry.

Canonical reason taxonomy:
- `ProviderError` — `UyuniProvider` couldn't be resolved or reached
- `ResolveRefs` — internal resolution error (k8s API issue, generally)
- `ReferenceUnavailable` — referenced k8s resource is missing or inconsistent at runtime
- `WaitingForChannel` / `WaitingForEnvironmentBuild` / `WaitingForRegistration` /
  `WaitingForSystemGroup` / `WaitingForConfigChannel` / `WaitingForDependents` —
  recoverable wait states, requeue and try again
- `CreateFailed` / `UpdateFailed` / `BuildFailed` / `ScheduleFailed` /
  `PromoteFailed` — Uyuni API call failed
- `InUse` — delete blocked by referencing resource
- `PromotionInFlight` — delete blocked by active promotion
- `DuplicateDefault` — `UyuniProvider` invariant violation
- `AdoptionTimedOut` — system never registered within `AdoptionTimeout`
- `Reconciled` — happy path; the only `True` reason

### 5. Conditions: Ready, UyuniDrift, BuildHost

Standard condition types and their semantics:

- `Ready` — overall reconciliation state. The above reasons populate this.
- `UyuniDrift` — `True` when an immutable field in Uyuni differs from spec.
  Webhook prevents customer-driven drift; this surfaces external drift
  (WebUI edits). Doesn't block reconciliation of mutable fields.
- `BuildHost` (System only) — `True` when system has `osimage_build_host`
  or `container_build_host` add-on active. Customer's `kubectl get system`
  printcolumn surfaces this.
- `PreProvisioned` (System only) — `True` between profile creation and
  first registration. Different from `Ready` because the system isn't
  actually managed yet.

### 6. Owner refs do cascading

Deleting a `ContentProject` triggers Kubernetes garbage collection of
referencing `ActivationKey` and `System` resources via `ownerReferences`
with `BlockOwnerDeletion: true`. **Do not reinvent cascade logic in
reconcilers.** Set ownership at resolve time
(`reconcileProjectOwnership`), trust the GC.

The pattern is fully documented in `internal/controller/ownership.go`.
When you reference a project, you own up to it; when you stop
referencing, you prune.

### 7. The webhook is mandatory, not optional

`failurePolicy: Fail`. If the webhook is unreachable, admission rejects.
This is correct: silently letting bad CRs through defeats the purpose.

Customer's install docs require cert-manager. The operator's deployment
manifest mounts the webhook TLS secret from a `Certificate` managed by
cert-manager via a self-signed `Issuer`. Don't try to self-manage certs
in the operator binary.

---

## How to add a new feature

The shape varies but the rhythm is consistent. Pick the bucket that
matches your feature.

### Adding a new field to an existing CR

1. Add the field to the type in `api/v1alpha1/*_types.go`. Use
   `+kubebuilder` markers for validation (Enum, Pattern, Minimum, etc.)
   when they're sufficient.
2. If the field is immutable post-create, add an immutability check to
   the webhook's `ValidateUpdate`.
3. If the field needs cross-field validation (e.g. "this requires that
   other field set"), add to the webhook's `validate()`.
4. Surface in the reconciler: read from spec, push to Uyuni, reflect in
   status if the API returns a confirmation value.
5. If immutable in Uyuni but mutable here would cause drift: add a
   `UyuniDrift` check on top of the immutability webhook.
6. Update the relevant sample manifest if the field is commonly used.
7. CHANGELOG entry under `### Added`.

### Adding a new resource type

1. Write the type in `api/v1alpha1/<resource>_types.go`. Include the
   `init()` function registering with `SchemeBuilder`.
2. Add finalizer constant to `internal/controller/constants.go`.
3. Write the reconciler in `internal/controller/<resource>_controller.go`.
   Use the existing reconcilers as templates. Mandatory pieces:
   - `migrateAnnotations` at top
   - Provider resolution via `r.Clients.For(...)`
   - Finalizer add/check/remove using `ensureFinalizer`/`containsFinalizer`/`removeFinalizer`
   - Force-delete annotation handling
   - `setReady` with the right reason on every exit path
   - `RBAC` markers above the struct
4. Write the webhook in `internal/webhook/<resource>_webhook.go`.
   Validates structural concerns; defers cross-resource to advisory warnings.
5. Add interface methods to `internal/uyuni/api.go`. Implement on both
   `*Client` and `uyunitest.FakeAPI`.
6. Register in `main.go` (when that exists).
7. Sample manifest in `config/samples/`.
8. CHANGELOG entry.

### Adding a new task kind

Tasks use a polymorphic spec with a discriminator. To add e.g. `ApplyContentProject`:

1. Define `ApplyContentProjectSpec` in `api/v1alpha1/task_types.go`.
2. Add the field to `TaskSpec`.
3. Update `validation.TaskKindCount` and `validation.TaskSpec` to
   recognize and check the new kind.
4. Add a `case` to `TaskReconciler.scheduleByKind`.
5. Add the matching API client method (`ScheduleApplyContentProject`)
   to the `uyuni.API` interface.

### Adding annotation triggers

Customer-facing annotations (`uyuni.uyuni-project.org/...`):

1. Add the constant to `api/v1alpha1/annotations.go`.
2. If it's a security-critical "I really mean it" annotation, add to
   `validation.DangerousAnnotations` so webhook enforces strict `"true"`.
3. If it's a one-shot trigger (like `build-now`, `rerun`, `sync-now`),
   the reconciler should:
   - Detect the annotation
   - Take the action
   - Update status FIRST (so the action is durable)
   - Strip the annotation in a separate Update call
   - Tolerate crash-between-action-and-strip (idempotent on retry)
4. Document in CHANGELOG.

### Adding webhook validations

Order of preference for *where* to put a check:

1. **OpenAPI markers** (`+kubebuilder:validation:Enum`, `Pattern`,
   `Minimum`, etc.). Free, enforced by API server.
2. **Pure validation function** in `internal/validation/`. Testable
   without I/O, called from webhook.
3. **Webhook validator** with k8s client lookups for cross-resource
   checks. Returns warnings for "not found" (GitOps-friendly), errors
   for structural problems.
4. **Reconciler check** only for runtime state (referenced resource
   missing at reconcile time, race-window backstops).

---

## Working with the validation package

The package contract is: **no I/O, just spec inspection**. If a check
needs to query the API server, it belongs in the webhook validator,
not here.

Return type is always `field.ErrorList`. Build paths with `field.NewPath`
so error messages point at the offending field (`spec.environments[1].label`).

Test pattern: table-driven, one struct per case with `name`, input spec,
`wantErrs int`, optional `wantPath string`. Tests run in milliseconds;
add a case for every edge condition.

---

## Working with the controller package

### Reconciler skeleton

```go
func (r *FooReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var foo uyuniv1.Foo
    if err := r.Get(ctx, req.NamespacedName, &foo); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 1. Annotation migration (upgrade hook, no-op fast path)
    if migrateAnnotations(&foo) {
        return ctrl.Result{}, r.Update(ctx, &foo)
    }

    // 2. Provider resolution
    uc, err := r.Clients.For(ctx, foo.Spec.ProviderRef, foo.Namespace)
    if err != nil {
        return r.fail(ctx, &foo, "ProviderError", err)
    }

    // 3. Deletion path
    if !foo.DeletionTimestamp.IsZero() {
        return r.handleDeletion(ctx, uc, &foo)
    }
    if ensureFinalizer(&foo, fooFinalizer) {
        return ctrl.Result{Requeue: true}, r.Update(ctx, &foo)
    }

    // 4. Resolve refs (channel resolver, group resolver, etc.)
    // 5. Maintain owner refs (for cascade deletion)
    // 6. Find-or-create in Uyuni
    // 7. Reconcile drift on mutable fields
    // 8. Update status, return requeue interval
}
```

### Requeue cadence

- In-flight build / pending registration: 15-30s
- Waiting on referenced resource: 30s
- Steady-state heartbeat (drift detection): 5m
- Cron-driven (build schedule, etc.): time until next deadline
- Provider reachability check: 2m

### When NOT to use `controllerutil.AddFinalizer`

The standard library's finalizer helpers don't know about our legacy
`uyuni.io/*` aliases. Always use the local `ensureFinalizer`,
`containsFinalizer`, `removeFinalizer` so legacy CRs migrate cleanly.

### Adding a watch

Cross-resource reactivity matters for fast convergence. The pattern:

```go
func (r *FooReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&uyuniv1.Foo{}).
        Watches(&uyuniv1.Bar{}, handler.EnqueueRequestsFromMapFunc(r.foosForBar)).
        Complete(r)
}

func (r *FooReconciler) foosForBar(ctx context.Context, obj client.Object) []reconcile.Request {
    var list uyuniv1.FooList
    if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
        return nil
    }
    var out []reconcile.Request
    for _, foo := range list.Items {
        if foo.references(obj.GetName()) {
            out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
                Namespace: foo.Namespace, Name: foo.Name,
            }})
        }
    }
    return out
}
```

The mapper lists are cache-backed (controller-runtime), not API calls,
so they're cheap. Don't preoptimize with field indexers unless profiling
shows it matters.

---

## Working with the webhook package

### Validator skeleton

```go
type FooValidator struct {
    Client client.Client
}

var _ webhook.CustomValidator = &FooValidator{}

func (v *FooValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
    v.Client = mgr.GetClient()
    return ctrl.NewWebhookManagedBy(mgr).
        For(&uyuniv1.Foo{}).
        WithValidator(v).
        Complete()
}

func (v *FooValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    return v.validate(ctx, obj.(*uyuniv1.Foo))
}

func (v *FooValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
    oldFoo := oldObj.(*uyuniv1.Foo)
    newFoo := newObj.(*uyuniv1.Foo)
    // Immutability checks here, fail-fast before validate()
    if oldFoo.Spec.ImmutableField != newFoo.Spec.ImmutableField {
        return nil, apierrors.NewForbidden(...)
    }
    return v.validate(ctx, newFoo)
}

func (v *FooValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
    return nil, nil
}

func (v *FooValidator) validate(ctx context.Context, foo *uyuniv1.Foo) (admission.Warnings, error) {
    errs := validation.SomeStructuralCheck(foo.Spec, field.NewPath("spec"))
    errs = append(errs, validation.StrictBooleanAnnotations(...))
    // ... cross-resource checks producing warnings or errors
    if len(errs) > 0 {
        return warnings, apierrors.NewInvalid(
            schema.GroupKind{Group: uyuniv1.Group, Kind: "Foo"},
            foo.Name, errs)
    }
    return warnings, nil
}
```

### Webhook marker checklist

The `+kubebuilder:webhook:` marker needs all of these for the generator
to produce correct manifests:

- `path=/validate-uyuni-uyuni-project-org-v1alpha1-<resource>` (dashes in path!)
- `mutating=false` (or `true` for defaulters)
- `failurePolicy=fail`
- `sideEffects=None`
- `groups=uyuni.uyuni-project.org`
- `resources=<plural>` (lowercase)
- `verbs=create;update` (don't include delete unless you actually validate it)
- `versions=v1alpha1`
- `name=v<resource>.uyuni.uyuni-project.org`
- `admissionReviewVersions=v1`

### Cross-resource validation: warning vs error

- Resource exists in spec but k8s API returns NotFound: **warning**.
  GitOps applies bundles, ordering isn't guaranteed.
- Resource exists, but the referenced sub-field doesn't (e.g. environment
  not in project): **error**. Almost certainly a typo.
- API server unreachable: **silently succeed**. Don't block admission on
  infrastructure problems.

---

## Working with the API client

### Adding a Uyuni API method

1. Add to the `API` interface in `internal/uyuni/api.go`.
2. Implement on `*Client` (TODO: file location once HTTP client lands).
3. Implement on `uyunitest.FakeAPI` — the fake should mirror Uyuni's
   semantics, not just return canned values. If Uyuni rejects a call
   because of state X, the fake should too.
4. If the method involves a Uyuni error type we don't already recognize,
   teach `IsNotFound` (or add a new error predicate) to detect it.

### Error handling conventions

- Use `uyuni.IsNotFound(err)` to detect 404-equivalent faults.
- Use `errors.As` to extract typed errors like `*SystemExistsError` for
  the adoption-on-create-conflict pattern.
- Network errors and other transient failures: return as-is; controller
  retries via requeue.

---

## Group rename: still in transition

The API group moved from `uyuni.io` to `uyuni.uyuni-project.org`.

Migration mechanics already in place:

1. `internal/controller/constants.go` holds `legacyAliases` (finalizers)
   and `legacyAnnotationMap` (annotation keys).
2. `ensureFinalizer`/`containsFinalizer`/`removeFinalizer` recognize both.
3. `migrateAnnotations` promotes legacy annotations on every reconcile.

**Plan**: drop both legacy maps and the migration code one minor version
after the rename ships. The TODO comments are tagged `post-v0.x`.

If you find yourself writing new code that references `uyuni.io` as a
literal string outside of these alias maps, you're doing something wrong.
The shared constants in `api/v1alpha1/annotations.go` and the
`uyuniv1.Group` constant should always be used.

---

## Common pitfalls

### Don't write a custom cascade

Use `ownerReferences`. The earlier design had a `cascade-delete`
annotation that triggered reconciler-driven dependent deletion. It was
deleted in favor of Kubernetes-native GC. If you find yourself listing
dependents to delete them, you're reinventing GC.

Exception: blocking delete with `Ready=False / InUse` is fine when the
relationship isn't owner-shaped (e.g. SoftwareChannel ↔ Repository:
they're peers, not parent-child).

### Don't validate structurally in reconcilers

If a reconciler check rejects spec on grounds that don't depend on
runtime state, that check belongs in the webhook. Reconciler checks
that duplicate webhook validation become dead code over time and
accumulate bugs.

The exception is webhook-bypass diagnostics: keeping a narrow check
with a `"admission should have rejected, check webhook configuration"`
message is fine. It's a smoke alarm, not validation.

### Don't conflate Ready and Available

`Ready=True` means "the operator did what the spec asks." It does NOT
mean "the underlying thing is healthy in Uyuni." If an environment is
built but the build had errors, `Ready=True / BuildStatus=Failed` is
correct.

### Don't use `controllerutil.AddFinalizer`

It doesn't know about legacy aliases. Always `ensureFinalizer`. Same for
the matching `Contains` and `Remove` variants.

### Don't strip annotations before acting

The order for annotation triggers is:

1. Detect annotation
2. Take the action (schedule task, kick build, etc.)
3. Update status to record what was done
4. *Then* strip the annotation in a separate Update

Reversing 3 and 4 risks lost intent on crash. Doing 4 before 2 means
duplicate work on retry, but no data loss — that's the safe direction.

### Don't fail on Uyuni-side drift

If `current.X != spec.X` and X is immutable in Uyuni, this is *not* a
reconcile error. Set `UyuniDrift=True` and continue reconciling mutable
fields. The customer needs to see drift, not have their CR stuck.

### Don't put cluster-scoped owner refs on namespaced objects

`UyuniProvider` is cluster-scoped. ActivationKeys etc. cannot have owner
refs to it (Kubernetes forbids namespaced → cluster-scoped ownership
in this direction). If you find yourself trying to do this, you're
working around a different problem.

### Don't read secrets cross-namespace

`UyuniProvider`'s credential secret must live in the operator's own
namespace (default: `uyuni-operator-system`). The pool enforces this.
Don't add a mechanism to read user-namespace secrets — that's a privilege
escalation surface.

---

## Testing

### Three test surfaces

1. **`internal/validation/`** — pure functions, table-driven, no
   dependencies. Fast (<1s for the whole package). Run on every CI run.
2. **`internal/controller/`** — reconciler logic against the state-based
   `uyunitest.FakeAPI` and controller-runtime's fake k8s client. Tests
   one Reconcile pass at a time; assert resulting CR + Uyuni state.
3. **`internal/webhook/`** — envtest-based. Spins up an embedded etcd +
   API server, registers our webhooks, drives kubectl-equivalent operations.

### Reconciler test pattern

```go
fakeAPI := uyunitest.New()
fakeAPI.SeedRegistered("web-01.example.com", "")

k8s := fake.NewClientBuilder().
    WithScheme(scheme).
    WithObjects(&sys).
    WithStatusSubresource(&sys).  // ALWAYS — without this, status writes silently succeed
    Build()

r := &controller.SystemReconciler{
    Client:  k8s,
    Clients: fakeAPI.Pool(),
    Now:     func() time.Time { return fixedTime },
}
_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ...})

// Assert: CR status, conditions, Uyuni-side state via fakeAPI accessors
```

The `WithStatusSubresource` line is non-optional. Without it, the fake
client allows `client.Update` to overwrite status (which the real API
server forbids), so your tests pass with code that fails in production.

### Fake API conventions

- Seed methods (`SeedRegistered`, `SeedProject`, `SeedProjectEnv`) set up
  initial Uyuni state.
- Setter methods (`SetActionStatus`, `PromoteToRegistered`) simulate
  external state changes (Uyuni completes a build, system phones home).
- Assertion methods (`HasProject`, `ProjectEnvLabels`, `HasBuildCall`)
  for terse test expectations.
- Failure injection via `ErrOn*` fields for testing error paths.

### Time injection

Every reconciler with time-dependent logic takes a `Now func() time.Time`
field. Tests pass a fixed time; production passes `time.Now`. Don't call
`time.Now()` directly inside a reconciler.

---

## Build and run

The Makefile is the entry point for every dev task. `make help` lists
all targets grouped by purpose.

```bash
# First-time setup downloads pinned tools into bin/
make generate manifests        # DeepCopy methods + CRDs + RBAC + webhook configs

# Fast feedback loop
make test                      # validation + controller-fake tests, ~seconds
make test-webhook              # envtest-based admission tests, ~30s startup
make test-all                  # both
make verify                    # CI gate: regenerate, test, fail on uncommitted diff

# Local run against current kubectl context
make install                   # apply CRDs (no operator yet)
make run                       # run operator out-of-cluster, watching cluster

# Container build + cluster deploy
make docker-build docker-push IMG=ghcr.io/you/uyuni-operator:tag
make deploy IMG=ghcr.io/you/uyuni-operator:tag

# Tear down
make undeploy
make uninstall
make clean                     # bin/ + coverage output
make clean-all                 # also envtest binary caches
```

Tool versions are pinned in the Makefile (`CONTROLLER_TOOLS_VERSION`,
`KUSTOMIZE_VERSION`, etc.) and downloaded to `bin/` on demand. Don't
rely on system installs of `controller-gen` or `kustomize` — version
mismatches against `controller-runtime` produce subtly wrong manifests.

What's still pending: `cmd/main.go` (the manager binary entry point —
mechanical kubebuilder wiring of pool + reconcilers + webhooks) and
the kustomize bases under `config/default`, `config/manager`,
`config/crd` (kubebuilder scaffold).

Once `cmd/main.go` and the kustomize tree land, `make run` and `make
deploy` work end-to-end.

---

## What's NOT in this repo

These are deliberately deferred:

- **`cmd/main.go`** — straightforward kubebuilder wiring; instantiate
  pool, register reconcilers and webhooks.
- **`config/default`, `config/manager`, `config/crd` kustomize bases** —
  generated by kubebuilder scaffold. The Makefile assumes their layout.
- **HTTP `*Client` implementation** — sketched in earlier design discussion;
  session auth via `pxt-session-cookie`, JSON over `/rhn/manager/api/`,
  transparent re-auth on 401.
- **`uyunitest.FakeAPI` body** — directory exists, state-based fake to be
  implemented matching the interface.
- **Conversion webhook** between old (`uyuni.io`) and new
  (`uyuni.uyuni-project.org`) CRDs — explicitly skipped. "Both CRDs
  registered" is the migration approach.
- **Helm chart** — should wrap the kustomize manifests, expose
  `operatorNamespace`, `image`, `replicaCount`, and cert-manager
  issuer reference.

---

## When in doubt

- Read the CHANGELOG before changing reason strings or annotation keys.
  Customer alerts depend on them.
- Read `RECONCILER_PATCHES.md` for the intent behind cleanup-pass changes
  to reconcilers that already existed.
- The `internal/validation/` tests are the canonical examples of "good
  validation tests" — copy that table-driven style.
- If a design choice surprises you, check git blame / commit messages.
  Most non-obvious decisions have rationale captured nearby.
- Customer's stack: RKE2 + Flux + cert-manager + Uyuni 2025.10+. Don't
  add hard dependencies outside that profile without flagging.

The operator should feel like a "system of record" for Uyuni state to
the customer. They write specs in git; we reconcile; status reflects
reality; drift gets surfaced honestly. Anything that compromises that
trust contract is a bug.
