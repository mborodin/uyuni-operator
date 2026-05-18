# uyuni-operator

Kubernetes-native control plane for [Uyuni](https://www.uyuni-project.org/),
modeled as CRDs you can manage through Flux/Argo/Helm/plain kubectl. Pairs
with cbosdo/uyuni-operator (which deploys Uyuni itself); this operator
configures *what's inside* a running Uyuni: channels, activation keys,
systems, content lifecycle projects, scheduled tasks, image builds, and so
on.

## Status

Pre-1.0. API group is `uyuni.uyuni-project.org/v1alpha1`. The shape is
stable enough that a v1beta1 conversion will only be additive, but expect
field renames between alphas if anything proves wrong in practice.

## Install

Prerequisites:

* Kubernetes 1.27+ (admission webhook `admissionReviewVersions: v1` only)
* [cert-manager](https://cert-manager.io/) for webhook TLS

```bash
kubectl create namespace uyuni-operator-system
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
# wait for cert-manager to be Ready...
kubectl apply -k github.com/mborodin/uyuni-operator/config/default
```

Then configure your Uyuni connection:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: uyuni-prod-creds
  namespace: uyuni-operator-system
type: Opaque
stringData:
  username: automation
  password: ...
---
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: UyuniProvider
metadata:
  name: prod
spec:
  url: https://uyuni.customer.example
  credentialsSecretRef:
    name: uyuni-prod-creds
    namespace: uyuni-operator-system
  isDefault: true
```

## CRDs

| Resource | What it manages |
| --- | --- |
| `UyuniProvider` (cluster) | Connection to a Uyuni server, credentials, default routing |
| `SoftwareChannel` | A software channel including GPG, parent, sync schedule |
| `Repository` | A repository attached to one or more channels |
| `ActivationKey` | Bootstrap keys for client registration |
| `System` | A managed system, with pre-create support for image-based provisioning |
| `SystemGroup` | A logical group of systems |
| `ConfigChannel` + `ConfigFile` | Salt state/normal/dictionary channels and their contents |
| `ImageStore` + `ImageProfile` | Image storage and kiwi/dockerfile build profiles |
| `ContentProject` + `ContentProjectPromotion` | CLM project, environments, filters; promotions as Job-shaped CRs |
| `Task` | Scheduled actions: highstate apply, remote command, reboot, patch install, config deploy |

See `config/samples/end-to-end.yaml` for a complete worked example.

## Annotations

* `uyuni.uyuni-project.org/force-delete` — skip Uyuni-side cleanup on
  finalizer run. Use when Uyuni is unreachable. Must be `"true"` exactly.
* `uyuni.uyuni-project.org/rerun` — trigger a Task to run again. Stripped
  after the run is recorded.
* `uyuni.uyuni-project.org/build-now` — trigger an ImageProfile build.
  Stripped after the build is scheduled.
* `uyuni.uyuni-project.org/sync-now` — trigger a SoftwareChannel sync.
* `uyuni.uyuni-project.org/build-version` — pin the next ImageProfile
  build's version string.

## Design notes

* **Webhooks for fast feedback, reconcilers for correctness**: structural
  validation rejects bad CRs at admission; reconcilers handle runtime
  failures and drift. Reconcilers don't duplicate spec validation; they
  do retain defense-in-depth checks for cases where webhooks were bypassed
  or referenced resources disappeared at runtime.

* **`UyuniDrift` condition**: surfaces WebUI-driven state changes that the
  operator can't reconcile away (e.g., changed immutable fields). Doesn't
  block `Ready`; meant for alerting on out-of-band changes.

* **Owner references for cascading delete**: `ActivationKey` and `System`
  take a non-controller owner ref on any referenced `ContentProject`, with
  `blockOwnerDeletion=true`. Kubernetes' GC handles the cascade; the
  project's finalizer waits until dependents finish their own cleanup
  before deleting the project in Uyuni.

* **Force-delete escape hatch**: every finalizer honors the force-delete
  annotation. Use sparingly and document the resulting orphan in Uyuni.

## Development

```bash
make generate manifests   # codegen + CRD/webhook/RBAC manifests
make test                 # validation + reconciler unit tests
make test-e2e             # envtest-backed webhook + integration tests
make install              # CRDs into current kubeconfig context
make run                  # operator locally, watching current cluster
```

## License

Apache 2.0.
