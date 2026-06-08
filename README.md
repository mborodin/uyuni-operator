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
| `ContentProject` | CLM project, filters, build settings (environments managed separately) |
| `ClmEnvironment` | CLM project environments (dev/test/prod), promotion chains |
| `ContentProjectPromotion` | Promotions between environments as Job-shaped CRs |
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

## Managing System Groups

The `SystemGroup` resource allows you to manage Uyuni system groups declaratively through Kubernetes with multi-cluster support.

### Quick Start

```bash
# Create a new system group
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: SystemGroup
metadata:
  name: web-servers
  namespace: linux-platform
spec:
  name: Web Servers
  description: Production web server fleet
  organizationRef:
    name: pantheon-of-goods
EOF

# Check status
kubectl get systemgroup -n linux-platform -o wide
kubectl describe systemgroup web-servers -n linux-platform
```

### Add a New System Group

```bash
# Method 1: Using kubectl apply with inline YAML
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: SystemGroup
metadata:
  name: app-servers
  namespace: linux-platform
spec:
  name: Application Servers
  description: Production application server fleet
  organizationRef:
    name: pantheon-of-goods
EOF

# Method 2: From a file
kubectl apply -f config/samples/systemgroup-sample.yaml

# Verify it was created
kubectl get systemgroup -n linux-platform
kubectl describe systemgroup app-servers -n linux-platform
```

**Expected Output:**
```
Status:
  Uyuni Id: 123
  Member Count: 0
  Conditions:
  - Type: Ready
    Status: True
    Reason: Reconciled
```

### Update a System Group

You can update the `description` field after creation:

```bash
# Update description
kubectl patch systemgroup web-servers -n linux-platform \
  --type merge -p '{"spec":{"description":"Updated: Production web servers v2"}}'

# Verify changes
kubectl describe systemgroup web-servers -n linux-platform
```

**Immutable Fields (Cannot be changed):**
- `spec.name` — Group name in Uyuni (changing would orphan the group)
- `spec.cluster` — Provider/cluster selection is permanent

If you need to change an immutable field, delete and recreate the resource.

### Add Systems to a Group

You can add systems in two ways:

**Method 1: Reference existing System resources**
```yaml
spec:
  memberRefs:
    - name: web-server-1
    - name: web-server-2
```

**Method 2: Use static minion IDs**
```yaml
spec:
  staticMinionIds:
    - web-server-1.example.com
    - web-server-2.example.com
```

### Multi-Cluster System Groups

For environments with multiple Uyuni instances, use the `cluster` field:

```yaml
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: SystemGroup
metadata:
  name: prod-servers
  namespace: linux-platform
spec:
  name: Production Servers
  description: Servers managed by production Uyuni instance
  organizationRef:
    name: pantheon-of-goods
  cluster:
    name: prod-uyuni  # References a specific UyuniProvider
```

The `cluster` field is immutable and determines which Uyuni instance manages this group.

### Delete a System Group

```bash
# Normal delete (removes from both Kubernetes and Uyuni)
kubectl delete systemgroup web-servers -n linux-platform
```

**What happens:**
1. Operator sees deletion timestamp
2. Removes group from Uyuni automatically
3. Removes finalizer after Uyuni cleanup
4. Resource deleted from Kubernetes

### Force Delete (Skip Uyuni Cleanup)

Use when Uyuni is unreachable but you want to remove the Kubernetes resource:

```bash
# Add force-delete annotation
kubectl annotate systemgroup web-servers \
  -n linux-platform \
  uyuni.uyuni-project.org/force-delete=true \
  --overwrite

# Delete
kubectl delete systemgroup web-servers -n linux-platform
```

**What happens:**
1. Operator skips Uyuni deletion
2. Group remains in Uyuni (orphaned)
3. Finalizer removed
4. Resource deleted from Kubernetes

### Monitor Status

```bash
# Watch real-time changes
kubectl get systemgroup -n linux-platform -w

# Check detailed status
kubectl describe systemgroup web-servers -n linux-platform

# View operator logs
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f

# Check groups in Uyuni UI
# Navigate to: https://your-uyuni/rhn/manager/do/groups/list
```

### Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| `Ready: False` | Configuration error or missing org | Check `kubectl describe` for error message |
| `spec.name is immutable` | Tried to change group name | Delete and recreate with new name |
| `spec.cluster is immutable` | Tried to change provider/cluster | Delete and recreate with new cluster |
| Resource stuck in deletion | Finalizer blocking | Use force-delete annotation |
| Members not syncing | System resources not ready | Ensure System CRs exist and have UyuniServerID |

## Managing Content Projects

The `ContentProject` resource allows you to manage Uyuni Content Lifecycle Management (CLM) projects declaratively. Environments are now managed separately via `ClmEnvironment` CRDs.

### Quick Start

```bash
# Create a new content project (without environments field)
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ContentProject
metadata:
  name: leap-platform
  namespace: linux-platform
spec:
  label: leap-platform                    # Immutable project identifier
  name: openSUSE Leap Platform            # Display name
  description: Leap 15.6 package builds
  sourceRefs:
    - name: opensuse-leap-15-6-x86-64    # Reference to SoftwareChannel
  organizationRef:
    name: pantheon-of-goods
EOF

# Check status
kubectl get contentproject -n linux-platform -o wide
kubectl describe contentproject leap-platform -n linux-platform
```

### Create a Project with Filters

```bash
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ContentProject
metadata:
  name: filtered-project
  namespace: linux-platform
spec:
  label: filtered-project
  name: Filtered Content
  description: Project with package filters
  sourceRefs:
    - name: opensuse-leap-15-6-x86-64
  filters:
    - name: no-rc-kernels
      type: package
      rule: deny                          # Exclude matching packages
      criteria:
        field: name
        matcher: matches
        value: "kernel.*-rc[0-9]+"
    - name: critical-updates-only
      type: errata
      rule: allow                         # Include only critical updates
      criteria:
        field: advisory_type
        matcher: equals
        value: "Security Advisory"
  organizationRef:
    name: pantheon-of-goods
EOF
```

### Enable Auto-Build

```bash
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ContentProject
metadata:
  name: auto-build-project
  namespace: linux-platform
spec:
  label: auto-build-project
  name: Auto-Build Project
  description: Automatically builds when sources change
  sourceRefs:
    - name: opensuse-leap-15-6-x86-64
  build:
    autoBuildSources: true               # Build when source channels change
    schedule: "0 4 * * 1"                # Weekly build at 4 AM Monday
    message: "automated build by uyuni-operator"
  organizationRef:
    name: pantheon-of-goods
EOF
```

### Update a Content Project

You can update `name`, `description`, `filters`, and `build` settings:

```bash
# Update project name
kubectl patch contentproject leap-platform -n linux-platform \
  --type merge -p '{"spec":{"name":"openSUSE Leap 15.6 Platform"}}'

# Update description
kubectl patch contentproject leap-platform -n linux-platform \
  --type merge -p '{"spec":{"description":"Updated: Production builds for Leap"}}'

# Enable auto-build schedule
kubectl patch contentproject leap-platform -n linux-platform \
  --type merge -p '{"spec":{"build":{"schedule":"0 4 * * 1"}}}'
```

**Immutable Fields (Cannot be changed):**
- `spec.label` — Project identifier in Uyuni
- `spec.sourceRefs` — Source channels (recreate project to change)

### Managing Environments Separately

**Recommended approach:** Environments are now managed via separate `ClmEnvironment` CRDs. This gives you:
- **Single source of truth** — environments defined only in ClmEnvironment CRDs, not in ContentProject.spec
- **Independent lifecycle** — create/update/delete environments without affecting the project
- **Full DELETE automation** — deleting a ClmEnvironment automatically removes it from Uyuni UI
- **Cleaner architecture** — projects and promotion chains are separate concerns

**Important:** Do NOT use the `spec.environments` field in ContentProject anymore. It is now optional and kept only for backward compatibility. All environments must be managed via separate ClmEnvironment resources.

```bash
# After creating ContentProject, add environments via ClmEnvironment CRDs
kubectl apply -f - <<'EOF'
---
# Root environment (dev)
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: dev-env
  namespace: linux-platform
spec:
  id: dev
  name: Development
  description: Development environment
  projectRef:
    name: leap-platform
  organizationRef:
    name: pantheon-of-goods

---
# Test environment (promotes from dev)
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: test-env
  namespace: linux-platform
spec:
  id: test
  name: Testing
  description: Testing environment
  projectRef:
    name: leap-platform
  predecessor: dev                       # Promotes from 'dev'
  organizationRef:
    name: pantheon-of-goods

---
# Production environment (promotes from test)
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: prod-env
  namespace: linux-platform
spec:
  id: prod
  name: Production
  description: Production environment
  projectRef:
    name: leap-platform
  predecessor: test                      # Promotes from 'test'
  organizationRef:
    name: pantheon-of-goods
EOF
```

### Delete a Content Project

```bash
# Normal delete (removes from both Kubernetes and Uyuni)
kubectl delete contentproject leap-platform -n linux-platform
```

**What happens:**
1. Operator sees deletion timestamp on ContentProject
2. **Cascades to all ClmEnvironment resources** in that project
3. Removes all environments from Uyuni automatically
4. Removes project from Uyuni
5. Removes finalizers and deletes all resources from Kubernetes

**Note:** Deleting the ContentProject will also delete all associated `ClmEnvironment` resources. If you only want to remove specific environments, delete individual `ClmEnvironment` CRs instead (see next section).

### Monitor Status

```bash
# Watch content projects
kubectl get contentproject -n linux-platform -w

# Check detailed status
kubectl describe contentproject leap-platform -n linux-platform

# View operator logs
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f

# Check projects in Uyuni UI
# Navigate to: https://your-uyuni/rhn/manager/do/contentmanagement/projects
```

### Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| `Ready: False` | Source channel not found | Ensure SoftwareChannel exists, check `kubectl describe` |
| `spec.label is immutable` | Tried to change project label | Delete and recreate with new label |
| Build failed | Invalid filter criteria | Check YAML syntax, verify filter field/matcher values |
| Environments not appearing | Not using ClmEnvironment CRDs | Create separate ClmEnvironment resources for each environment |
| Project stuck in deletion | Environments have finalizers blocking | Delete environments first with `kubectl delete clmenvironment` |
| Manual UI cleanup needed after delete | Using force-delete annotation | Environments remain in Uyuni UI as orphans; manually clean up via UI if needed |

## Managing CLM Environments

The `ClmEnvironment` resource allows you to manage Uyuni Content Lifecycle Management (CLM) environments declaratively with multi-cluster support.

### Quick Start

**Prerequisites:** A `ContentProject` must be created first. See "Managing Content Projects" section.

```bash
# Create a new CLM environment
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: dev-env
  namespace: linux-platform
spec:
  id: dev                              # Immutable environment label
  name: Development
  description: Development environment
  projectRef:
    name: leap-platform                # Reference to parent ContentProject
  organizationRef:
    name: pantheon-of-goods
  # No cluster field → uses default UyuniProvider
EOF

# Check status
kubectl get clmenvironment -n linux-platform -o wide
kubectl describe clmenvironment dev-env -n linux-platform
```

**Note:** The `cluster` field is optional. Omit it to use the default UyuniProvider (faster for single Uyuni setups).

### Create Promotion Chain

Environments form a promotion chain: dev → test → prod

```bash
# Root environment (dev)
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: dev-env
  namespace: linux-platform
spec:
  id: dev
  name: Development
  projectRef:
    name: leap-platform
  organizationRef:
    name: pantheon-of-goods
EOF

# Test environment (promotes from dev)
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: test-env
  namespace: linux-platform
spec:
  id: test
  name: Testing
  projectRef:
    name: leap-platform
  predecessor: dev                     # Promotes from 'dev'
  organizationRef:
    name: pantheon-of-goods
EOF

# Production environment (promotes from test)
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: ClmEnvironment
metadata:
  name: prod-env
  namespace: linux-platform
spec:
  id: prod
  name: Production
  projectRef:
    name: leap-platform
  predecessor: test                    # Promotes from 'test'
  organizationRef:
    name: pantheon-of-goods
EOF
```

### Update an Environment

You can update the `name` and `description` fields after creation:

```bash
# Update description
kubectl patch clmenvironment dev-env -n linux-platform \
  --type merge -p '{"spec":{"description":"Updated: Development v2"}}'

# Update name
kubectl patch clmenvironment dev-env -n linux-platform \
  --type merge -p '{"spec":{"name":"Dev Environment"}}'
```

**Immutable Fields (Cannot be changed):**
- `spec.id` — Environment label in Uyuni (uniquely identifies environment)
- `spec.projectRef` — Parent ContentProject
- `spec.cluster` — Provider/cluster selection

If you need to change an immutable field, delete and recreate the resource.

### Multi-Cluster Environments

For multiple Uyuni instances, specify different clusters:

```yaml
spec:
  id: prod
  name: Production
  projectRef:
    name: leap-platform
  cluster:
    name: prod-uyuni  # Reference different UyuniProvider
  predecessor: test
```

Omit the cluster field to use the default UyuniProvider:

```yaml
spec:
  id: dev
  name: Development
  projectRef:
    name: leap-platform
  # No cluster field → uses default UyuniProvider
```

### Delete an Environment

```bash
# Normal delete (removes from both Kubernetes and Uyuni UI)
kubectl delete clmenvironment dev-env -n linux-platform
```

**What happens:**
1. Operator sees deletion timestamp on ClmEnvironment
2. Sends HTTP DELETE request to Uyuni API with full environment object
3. Uyuni removes the environment from the project
4. Operator removes finalizer after successful Uyuni cleanup
5. Resource deleted from Kubernetes

**Result:** The environment is completely removed from both Kubernetes **and Uyuni UI**. No manual UI cleanup needed.

### Force Delete (Skip Uyuni Cleanup)

Use when Uyuni is unreachable but you want to remove the Kubernetes resource:

```bash
# Add force-delete annotation
kubectl annotate clmenvironment dev-env \
  -n linux-platform \
  uyuni.uyuni-project.org/force-delete=true \
  --overwrite

# Delete
kubectl delete clmenvironment dev-env -n linux-platform
```

### Monitor Status

```bash
# Watch real-time changes
kubectl get clmenvironment -n linux-platform -w

# Check detailed status
kubectl describe clmenvironment dev-env -n linux-platform

# View operator logs
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f

# Check environments in Uyuni UI
# Navigate to: https://your-uyuni/rhn/manager/do/contentmanagement/project_environments
```

### Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| `Ready: False` | Project not found or configuration error | Ensure ContentProject exists, check `kubectl describe` |
| `spec.id is immutable` | Tried to change environment ID | Delete and recreate with new ID |
| `spec.projectRef is immutable` | Tried to change parent project | Delete and recreate with new project |
| `spec.cluster is immutable` | Tried to change provider/cluster | Delete and recreate with new cluster |
| Resource stuck in deletion | Finalizer blocking | Use force-delete annotation |
| Predecessor not found | Referenced predecessor environment doesn't exist | Ensure predecessor environment is created first |
| Environment not deleted from Uyuni | DELETE request failed (check logs) | Run `kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f` to see DEBUG messages |
| Manual cleanup needed in Uyuni UI | Used force-delete annotation | Environment remains in Uyuni UI as orphan; manually remove via UI if needed |

## Complete DELETE Automation

The operator provides **full DELETE automation** — deleting resources from Kubernetes automatically removes them from Uyuni UI with zero manual steps.

### Environment Deletion Workflow

```bash
# Delete a single environment
kubectl delete clmenvironment dev-env -n linux-platform
```

**Automation flow:**
1. Kubernetes marks resource for deletion (adds deletion timestamp)
2. Operator detects finalizer and begins cleanup
3. Operator sends HTTP DELETE to Uyuni API with full environment object
4. Uyuni removes environment from project
5. Operator verifies success and removes finalizer
6. Kubernetes garbage collects the resource

**Result:** Environment is removed from both Kubernetes **and Uyuni UI** automatically. No manual UI cleanup needed.

### Project Deletion Workflow (With Cascade)

```bash
# Delete project and all its environments at once
kubectl delete contentproject leap-platform -n linux-platform
```

**Automation flow:**
1. Kubernetes marks ContentProject for deletion
2. Operator detects finalizer on ContentProject
3. Kubernetes garbage collector automatically deletes all child ClmEnvironment resources
4. Each ClmEnvironment deletion triggers its own DELETE to Uyuni API
5. After all environments are deleted, ContentProject DELETE is sent to Uyuni
6. All finalizers are removed
7. All resources cleaned up from Kubernetes

**Result:** Project and all environments removed from Kubernetes **and Uyuni UI** in a single cascade operation. No manual steps required.

### Selective Environment Deletion

```bash
# Delete only specific environments, keep project
kubectl delete clmenvironment test-env -n linux-platform
kubectl delete clmenvironment prod-env -n linux-platform
# dev-env remains
```

The ContentProject is unaffected. Only the deleted ClmEnvironment resources are removed from Uyuni.

### Force Delete (Emergency Only)

Use when Uyuni is unreachable but you want to clean up Kubernetes:

```bash
# Mark for force delete (skips Uyuni API call)
kubectl annotate clmenvironment dev-env -n linux-platform \
  uyuni.uyuni-project.org/force-delete=true --overwrite

# Delete from Kubernetes only
kubectl delete clmenvironment dev-env -n linux-platform
```

**What happens:**
- Operator skips HTTP DELETE to Uyuni
- Resource removed from Kubernetes immediately
- Environment remains in Uyuni UI as orphan
- **Manual cleanup in Uyuni UI required** (remove via Web UI)

Use sparingly and document any orphaned resources created this way.

### Monitoring DELETE Operations

Check operator logs to see DELETE automation in action:

```bash
# Watch DELETE operations in real-time
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -f | grep -E "DELETE|RemoveEnvironment|RemoveProject"
```

**Example log output:**
```
DEBUG: RemoveEnvironment called for project=leap-platform, env=dev
DEBUG: Sending DELETE request for env=dev with payload: map[...]
DEBUG: DELETE with body response: {"success":true,"messages":null,"errors":null,...}
DEBUG: Environment dev deleted successfully
```

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
