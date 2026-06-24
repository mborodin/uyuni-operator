# Uyuni-Operator Deployment Guide for AKS

Complete step-by-step guide to deploy uyuni-operator to Azure Kubernetes Service (AKS) using Flux CD.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Architecture Overview](#architecture-overview)
3. [Deployment Steps](#deployment-steps)
4. [Manual Configuration](#manual-configuration)
5. [Verification](#verification)
6. [Troubleshooting](#troubleshooting)

---

## Prerequisites

### Required Tools

- `kubectl` - configured to connect to your AKS cluster
- `flux` - Flux CLI tool (v2.x)
- Docker Hub account (for private image pull)
- Access to Uyuni server (URL, username, password)

### AKS Cluster Requirements

- Kubernetes 1.20+
- cert-manager installed (for webhook TLS certificates)
- Flux CD bootstrapped and ready

### Check Prerequisites

```bash
# Check kubectl connectivity
kubectl cluster-info

# Check Flux is installed
flux version

# Check cert-manager is running
kubectl get pods -n cert-manager

# Check Flux system namespace
kubectl get pods -n flux-system
```

---

## Architecture Overview

The deployment uses **three layers**:

### Layer 1: Automated (via Flux)
- Kubernetes Namespace (`uyuni-operator-system`)
- Operator Deployment (controller-manager)
- Custom Resource Definitions (CRDs)
- RBAC (Roles, RoleBindings, ServiceAccounts)
- Webhooks (ValidatingWebhook, MutatingWebhook)
- cert-manager resources (Certificate, Issuer)

### Layer 2: Semi-Automated (Flux manifest)
- GitRepository pointing to operator repo
- Kustomization pointing to `config/default`

### Layer 3: Manual (Manual kubectl commands)
- **Docker Hub Secret** - for pulling private operator image
- **Uyuni Credentials Secret** - for Uyuni API authentication
- **UyuniProvider Resource** - connection config to Uyuni server

---

## Deployment Steps

### Step 1: Prepare AKS Cluster

#### 1.1 Create a kubeconfig context (if needed)

```bash
# Set your Azure subscription
az account set --subscription YOUR_SUBSCRIPTION_ID

# Get AKS credentials
az aks get-credentials --resource-group YOUR_RESOURCE_GROUP --name YOUR_AKS_CLUSTER_NAME
```

#### 1.2 Verify Flux is bootstrapped

```bash
# Check if Flux system namespace exists
kubectl get namespace flux-system

# If not, bootstrap Flux
flux bootstrap github \
  --owner=YOUR_GITHUB_USERNAME \
  --repo=YOUR_FLUX_REPO \
  --path=clusters/YOUR_CLUSTER_NAME \
  --personal
```

#### 1.3 Verify cert-manager is installed

```bash
# Check cert-manager pods
kubectl get pods -n cert-manager

# If not installed, install it:
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.0/cert-manager.yaml

# Wait for it to be ready
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

---

### Step 2: Add Flux Manifest for Uyuni-Operator

#### 2.1 Create Flux manifests directory

```bash
mkdir -p YOUR_FLUX_REPO/clusters/YOUR_CLUSTER_NAME/uyuni-operator
cd YOUR_FLUX_REPO/clusters/YOUR_CLUSTER_NAME/uyuni-operator
```

#### 2.2 Create GitRepository manifest

```bash
cat > git-repository.yaml <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: GitRepository
metadata:
  name: uyuni-operator
  namespace: flux-system
spec:
  interval: 1m0s
  url: https://github.com/mborodin/uyuni-operator
  ref:
    branch: feat/dply-e2e-aks
EOF
```

#### 2.3 Create Kustomization manifest

```bash
cat > kustomization.yaml <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: uyuni-operator
  namespace: flux-system
spec:
  interval: 10m0s
  sourceRef:
    kind: GitRepository
    name: uyuni-operator
  path: ./config/default
  prune: true
  wait: true
  timeout: 10m0s
  healthChecks:
  - apiVersion: v1
    kind: Service
    name: webhook-service
    namespace: uyuni-operator-system
EOF
```

#### 2.4 Apply to cluster

```bash
# Apply the Flux manifests
kubectl apply -f git-repository.yaml
kubectl apply -f kustomization.yaml

# Watch Flux sync
kubectl get gitrepository uyuni-operator -n flux-system -w
kubectl get kustomization uyuni-operator -n flux-system -w
```

#### 2.5 Verify Flux deployment

```bash
# Check GitRepository synced successfully
kubectl get gitrepository uyuni-operator -n flux-system

# Should show: READY: True, STATUS: Fetched revision

# Check Kustomization is ready
kubectl get kustomization uyuni-operator -n flux-system

# Should show: READY: True, STATUS: Applied revision
```

---

### Step 3: Verify Operator Infrastructure Deployed

```bash
# Check namespace created
kubectl get namespace uyuni-operator-system

# Check operator pod is running
kubectl get pods -n uyuni-operator-system
kubectl get deployment -n uyuni-operator-system

# Check CRDs installed
kubectl get crd | grep uyuni

# Check RBAC resources
kubectl get sa,role,rolebinding -n uyuni-operator-system

# Check webhook service
kubectl get service webhook-service -n uyuni-operator-system

# Check webhook configurations
kubectl get validatingwebhookconfigurations | grep uyuni
kubectl get mutatingwebhookconfigurations | grep uyuni
```

---

## Manual Configuration

Once Flux deploys the operator infrastructure, you must **manually create three resources** because they contain credentials and environment-specific config.

### Step 4: Create Docker Hub Secret

Required for pulling the private operator image from Docker Hub.

```bash
kubectl create secret docker-registry dockerhub-secret \
  --docker-server=docker.io \
  --docker-username=YOUR_DOCKER_USERNAME \
  --docker-password=YOUR_DOCKER_PASSWORD \
  --docker-email=YOUR_EMAIL \
  -n uyuni-operator-system
```

**Verify:**
```bash
kubectl get secret dockerhub-secret -n uyuni-operator-system
```

---

### Step 5: Create Uyuni Credentials Secret

This contains the username and password for authenticating with your Uyuni server.

```bash
kubectl create secret generic uyuni-prod-creds \
  --from-literal=username=uyuni \
  --from-literal=password='YOUR_UYUNI_PASSWORD' \
  -n uyuni-operator-system
```

**Replace:**
- `YOUR_UYUNI_PASSWORD` - The Uyuni server password for user "uyuni"

**Verify:**
```bash
kubectl get secret uyuni-prod-creds -n uyuni-operator-system
```

---

### Step 6: Create UyuniProvider Resource

This tells the operator how to connect to your Uyuni server.

```bash
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: UyuniProvider
metadata:
  name: prod
  namespace: uyuni-operator-system
spec:
  # Uyuni server URL
  url: https://aks-uyuni-new.nexa-dev.delhaize.test
  
  # Reference to credentials secret created in Step 5
  credentialsSecretRef:
    name: uyuni-prod-creds
    namespace: uyuni-operator-system
  
  # Mark as default provider
  isDefault: true
  
  # API call timeout
  timeout: 30s
  
  # Skip TLS verification (for self-signed certs in dev/test)
  # IMPORTANT: Set to false in production with valid certificates
  insecureSkipVerify: true
EOF
```

**Replace:**
- `https://aks-uyuni-new.nexa-dev.delhaize.test` - Your Uyuni server URL
- `insecureSkipVerify: false` - Use true only for self-signed certs

---

## Verification

### Step 7: Verify Operator is Ready

```bash
# Check UyuniProvider is Ready
kubectl get uyuniprovider -n uyuni-operator-system

# Expected output:
# NAME   URL                                            VERSION   DEFAULT   READY
# prod   https://aks-uyuni-new.nexa-dev.delhaize.test   29        true      True
```

### Step 8: Check Operator Logs

```bash
# Get recent logs
kubectl logs -n uyuni-operator-system \
  -l control-plane=controller-manager \
  -c manager \
  --tail=50

# Watch logs in real-time
kubectl logs -n uyuni-operator-system \
  -l control-plane=controller-manager \
  -c manager \
  -f
```

### Step 9: Test with Sample Resource

Create a test Organization to verify the operator is working:

```bash
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: Organization
metadata:
  name: test-org
  namespace: uyuni-operator-system
spec:
  name: "Test Organization"
  description: "Testing uyuni-operator deployment"
  admin:
    username: admin
    password: "admin-password"
  defaultUser:
    username: testuser
    password: "user-password"
EOF

# Monitor creation
kubectl get organization test-org -n uyuni-operator-system -w

# Check status
kubectl describe organization test-org -n uyuni-operator-system
```

---

## Troubleshooting

### Issue: Flux Kustomization shows "Reconciliation in progress" or fails

**Check logs:**
```bash
kubectl logs -n flux-system -l app=kustomize-controller -f | grep uyuni-operator
```

**Common causes:**
- cert-manager not installed
- Webhook service not deployed
- RBAC permissions missing

**Solution:**
```bash
# Manually trigger reconciliation
flux reconcile kustomization uyuni-operator -n flux-system --with-source

# Check Flux sources
kubectl get gitrepository -n flux-system
```

---

### Issue: UyuniProvider shows READY=False or error

**Check UyuniProvider status:**
```bash
kubectl describe uyuniprovider prod -n uyuni-operator-system
```

**Check credentials exist:**
```bash
kubectl get secret uyuni-prod-creds -n uyuni-operator-system
kubectl get secret uyuni-prod-creds -n uyuni-operator-system -o yaml
```

**Common causes:**
1. Credentials secret doesn't exist → Create it (Step 5)
2. Wrong username/password → Recreate secret with correct values
3. Uyuni server unreachable → Check URL and network connectivity
4. TLS certificate issues → Check `insecureSkipVerify` setting

**Fix credentials:**
```bash
# Delete old secret
kubectl delete secret uyuni-prod-creds -n uyuni-operator-system

# Create new one with correct values
kubectl create secret generic uyuni-prod-creds \
  --from-literal=username=uyuni \
  --from-literal=password='CORRECT_PASSWORD' \
  -n uyuni-operator-system

# UyuniProvider will reconcile automatically
kubectl get uyuniprovider -n uyuni-operator-system -w
```

---

### Issue: Operator pod not starting or crashing

**Check pod status:**
```bash
kubectl get pods -n uyuni-operator-system
kubectl describe pod -n uyuni-operator-system -l control-plane=controller-manager
```

**Check image pull:**
```bash
# Verify dockerhub-secret exists
kubectl get secret dockerhub-secret -n uyuni-operator-system

# Check image pull errors in events
kubectl describe pod -n uyuni-operator-system -l control-plane=controller-manager | grep -A 10 Events
```

**Common causes:**
1. Docker secret missing → Create it (Step 4)
2. Wrong Docker credentials → Recreate secret
3. Image not available → Check image tag in kustomization

**Fix image pull:**
```bash
# Delete old secret
kubectl delete secret dockerhub-secret -n uyuni-operator-system

# Create with correct credentials
kubectl create secret docker-registry dockerhub-secret \
  --docker-server=docker.io \
  --docker-username=CORRECT_USERNAME \
  --docker-password=CORRECT_PASSWORD \
  --docker-email=YOUR_EMAIL \
  -n uyuni-operator-system

# Restart deployment to pull new image
kubectl rollout restart deployment/controller-manager -n uyuni-operator-system

# Watch restart
kubectl get pods -n uyuni-operator-system -w
```

---

### Issue: Webhook validation fails during resource creation

**Error example:**
```
failed calling webhook: service "webhook-service" not found
```

**Solution:**
```bash
# Verify webhook service exists
kubectl get service webhook-service -n uyuni-operator-system

# Check webhook configuration
kubectl get validatingwebhookconfigurations | grep uyuni

# If missing, redeploy Flux kustomization
flux reconcile kustomization uyuni-operator -n flux-system --with-source
```

---

### Issue: Leader election errors

**Check logs:**
```bash
kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -c manager | grep -i "leader\|lease"
```

**Error example:**
```
cannot get resource "leases" in API group "coordination.k8s.io": Azure does not have opinion for this user
```

**Solution:**
Verify RBAC rolebinding has correct namespace:

```bash
kubectl describe rolebinding leader-election-rolebinding -n uyuni-operator-system

# Should show:
# Subjects:
#   Kind            Name     Namespace
#   ----            ----     ---------
#   ServiceAccount  manager  uyuni-operator-system  ← MUST be uyuni-operator-system, not "system"
```

If namespace is wrong, redeploy via Flux (RBAC files have been fixed).

---

## Quick Reference: Complete Deployment

Copy and paste all commands in order:

```bash
# 1. Bootstrap Flux (if not already done)
flux bootstrap github \
  --owner=YOUR_GITHUB_USERNAME \
  --repo=YOUR_FLUX_REPO \
  --path=clusters/YOUR_CLUSTER_NAME \
  --personal

# 2. Apply Flux manifests
kubectl apply -f git-repository.yaml
kubectl apply -f kustomization.yaml

# 3. Wait for Flux to deploy operator (check status)
kubectl get kustomization uyuni-operator -n flux-system -w

# 4. Create Docker Hub secret
kubectl create secret docker-registry dockerhub-secret \
  --docker-server=docker.io \
  --docker-username=YOUR_DOCKER_USERNAME \
  --docker-password=YOUR_DOCKER_PASSWORD \
  --docker-email=YOUR_EMAIL \
  -n uyuni-operator-system

# 5. Create Uyuni credentials secret
kubectl create secret generic uyuni-prod-creds \
  --from-literal=username=uyuni \
  --from-literal=password='YOUR_UYUNI_PASSWORD' \
  -n uyuni-operator-system

# 6. Create UyuniProvider
kubectl apply -f - <<'EOF'
apiVersion: uyuni.uyuni-project.org/v1alpha1
kind: UyuniProvider
metadata:
  name: prod
  namespace: uyuni-operator-system
spec:
  url: https://aks-uyuni-new.nexa-dev.delhaize.test
  credentialsSecretRef:
    name: uyuni-prod-creds
    namespace: uyuni-operator-system
  isDefault: true
  timeout: 30s
  insecureSkipVerify: true
EOF

# 7. Verify deployment
kubectl get uyuniprovider -n uyuni-operator-system
kubectl get pods -n uyuni-operator-system
```

---

## Summary

| Component | Deployed By | Creation Method |
|-----------|-------------|-----------------|
| Namespace | Flux | Automatic |
| Operator Deployment | Flux | Automatic |
| CRDs | Flux | Automatic |
| RBAC | Flux | Automatic |
| Webhooks | Flux | Automatic |
| cert-manager resources | Flux | Automatic |
| Docker Hub Secret | **Manual** | kubectl create secret |
| Uyuni Credentials Secret | **Manual** | kubectl create secret |
| UyuniProvider | **Manual** | kubectl apply |

---

## Support

For issues not covered here:

1. Check operator logs: `kubectl logs -n uyuni-operator-system -l control-plane=controller-manager -c manager`
2. Check Flux logs: `kubectl logs -n flux-system -l app=kustomize-controller`
3. Verify all resources: `kubectl get all -n uyuni-operator-system`
4. Check events: `kubectl get events -n uyuni-operator-system --sort-by='.lastTimestamp'`

