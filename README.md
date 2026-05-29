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

## Managing Configuration Channels

The `ConfigurationChannel` resource allows you to manage Uyuni configuration channels (Salt state/normal/dictionary channels) declaratively through Kubernetes.

### Quick Start

```bash
# Create a new configuration channel
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: nginx-config
  namespace: linux-platform
spec:
  id: nginx-config-state              # Uyuni label (immutable)
  name: Nginx Configuration (state)   # Display name (mutable)
  type: state                         # Type: state, normal, or dictionary (immutable)
  description: Salt state for Nginx   # Required by Uyuni API
  url: https://github.com/example/salt-nginx  # Optional, for reference
EOF

# Check status
kubectl get configurationchannel -n linux-platform -o wide
kubectl describe configurationchannel nginx-config -n linux-platform
```

### Add a New Configuration Channel

```bash
# Method 1: Using kubectl apply with inline YAML
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: apache-config
  namespace: linux-platform
spec:
  id: apache-config-state
  name: Apache Configuration (state)
  type: state
  description: Apache web server configuration
EOF

# Method 2: From a file
kubectl apply -f my-config-channel.yaml

# Verify it was created
kubectl get configurationchannel -n linux-platform
kubectl describe configurationchannel apache-config -n linux-platform
```

**Expected Output:**
```
Status:
  Uyuni Id: 2
  Conditions:
  - Type: Ready
    Status: True
    Reason: Reconciled
  - Type: UyuniDrift
    Status: False
```

### Update a Configuration Channel

You can update the `name` and `description` fields after creation:

```bash
# Update description
kubectl patch configurationchannel nginx-config -n linux-platform \
  --type merge -p '{"spec":{"description":"Updated: Nginx Salt state v2"}}'

# Update name
kubectl patch configurationchannel nginx-config -n linux-platform \
  --type merge -p '{"spec":{"name":"Nginx Web Server Config"}}'

# Verify changes
kubectl describe configurationchannel nginx-config -n linux-platform
```

**Immutable Fields (Cannot be changed):**
- `spec.id` — Changing would orphan the channel in Uyuni
- `spec.type` — Type changes require recreating the channel
- `spec.cluster` — Provider selection is permanent

If you need to change an immutable field, delete and recreate the resource:

```bash
kubectl delete configurationchannel nginx-config -n linux-platform
# Then create with new values
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: nginx-config
  namespace: linux-platform
spec:
  id: nginx-config-normal    # Changed type
  name: Nginx Configuration (normal)
  type: normal               # Changed from state
  description: Nginx config
EOF
```

### Delete a Configuration Channel

```bash
# Normal delete (removes from both Kubernetes and Uyuni)
kubectl delete configurationchannel nginx-config -n linux-platform
```

**What happens:**
1. Operator sees deletion timestamp
2. Removes channel from Uyuni automatically
3. Removes finalizer after Uyuni cleanup
4. Resource deleted from Kubernetes

### Force Delete (Skip Uyuni Cleanup)

Use when Uyuni is unreachable but you want to remove the Kubernetes resource:

```bash
# Add force-delete annotation
kubectl annotate configurationchannel nginx-config \
  -n linux-platform \
  uyuni.uyuni-project.org/force-delete=true \
  --overwrite

# Delete
kubectl delete configurationchannel nginx-config -n linux-platform
```

**What happens:**
1. Operator skips Uyuni deletion
2. Channel remains in Uyuni (orphaned)
3. Finalizer removed
4. Resource deleted from Kubernetes

### Monitor Status

```bash
# Watch real-time changes
kubectl get configurationchannel -n linux-platform -w

# Check detailed status
kubectl describe configurationchannel nginx-config -n linux-platform

# View operator logs
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f

# Check sync to Uyuni
# Navigate to: https://your-uyuni/rhn/manager/do/channels/list/all
# You should see your configuration channels listed
```

### Namespace Considerations

All ConfigurationChannel resources in the same namespace **share the same UyuniProvider**. By default, the operator looks for a UyuniProvider with `spec.isDefault: true` in the resource's namespace.

**Recommended Setup:**
```bash
# Use a single namespace for related channels
linux-platform/
  ├── UyuniProvider (created in uyuni-operator-system)
  ├── nginx-config
  ├── apache-config
  ├── base-config
  └── database-config
```

If you need different Uyuni instances or team isolation:

```bash
# Create namespace-specific UyuniProvider
kubectl create namespace linux-prod
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: UyuniProvider
metadata:
  name: prod
  namespace: linux-prod
spec:
  url: https://uyuni-prod.example
  credentialsSecretRef:
    name: uyuni-prod-creds
    namespace: linux-prod
  isDefault: true
EOF
```

### Common Examples

**Example 1: Create a State Channel**
```yaml
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: webserver-config
  namespace: linux-platform
spec:
  id: webserver-state
  name: Web Server Configuration
  type: state
  description: Salt state files for web servers
  url: https://github.com/myorg/webserver-saltstate
```

**Example 2: Create a Normal Channel**
```yaml
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: system-defaults
  namespace: linux-platform
spec:
  id: system-defaults
  name: System Defaults
  type: normal
  description: Default system configuration files
```

**Example 3: Create a Dictionary Channel**
```yaml
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ConfigurationChannel
metadata:
  name: pillar-data
  namespace: linux-platform
spec:
  id: pillar-data
  name: Pillar Data
  type: dictionary
  description: Salt pillar data for all systems
```

### Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| `Ready: False` | Configuration error | Check `kubectl describe` for error message |
| `UyuniDrift: True` | Type mismatch in Uyuni | Delete and recreate with correct type |
| Resource stuck in deletion | Finalizer blocking | Use force-delete annotation |
| `description is required` | Empty description field | Add non-empty description to spec |
| `spec.id is immutable` | Tried to change ID | Delete and recreate with new ID |

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
