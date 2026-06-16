#!/usr/bin/env bash
# Usage: ./cleanup-brandregion.sh <claim-name> <namespace>
#   e.g: ./cleanup-brandregion.sh delhaize-apac linux-platform
#
# Flow:
#   1. Delete BrandRegionClaim → Crossplane cascades:
#        XBrandRegion → provider-kubernetes Objects → uyuni CRDs
#   2. Wait for Crossplane to finish
#   3. Force-remove finalizers on any stuck uyuni CRDs (Uyuni API unreachable)
set -euo pipefail

CLAIM="${1:?Usage: $0 <claim-name> <namespace>}"
NAMESPACE="${2:?Usage: $0 <claim-name> <namespace>}"

echo "==> Deleting BrandRegionClaim $CLAIM in $NAMESPACE..."
kubectl delete brandregionclaim "$CLAIM" -n "$NAMESPACE" --ignore-not-found

echo "==> Waiting 15s for Crossplane cascade (XBrandRegion → Objects → uyuni CRDs)..."
sleep 15

echo "==> Checking for stuck uyuni CRDs (Uyuni API unreachable = finalizer blocks)..."
for kind in clmenvironment contentproject activationkey systemgroup configurationchannel softwarechannel repository organization; do
  resources=$(kubectl get "$kind" -n "$NAMESPACE" --no-headers 2>/dev/null \
    | awk '{print $1}' \
    | grep "^${CLAIM}" || true)

  if [ -z "$resources" ]; then
    continue
  fi

  echo "==> Force-removing finalizers from stuck $kind resources..."
  for r in $resources; do
    kubectl patch "$kind/$r" -n "$NAMESPACE" \
      --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
  done
done

# UyuniProvider is cluster-scoped — check separately
prov=$(kubectl get uyuniprovider --no-headers 2>/dev/null \
  | awk '{print $1}' | grep "^${CLAIM}" || true)
if [ -n "$prov" ]; then
  echo "==> Force-removing finalizers from stuck UyuniProvider..."
  for r in $prov; do
    kubectl patch uyuniprovider/"$r" \
      --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
  done
fi

echo ""
echo "==> Remaining resources matching '$CLAIM':"
echo "--- Crossplane composites ---"
kubectl get brandregionclaim,xbrandregion 2>/dev/null \
  | grep "$CLAIM" || echo "    (none)"
echo "--- uyuni CRDs (linux-platform) ---"
kubectl get clmenvironment,contentproject,activationkey,systemgroup,\
configurationchannel,softwarechannel,repository,organization \
  -n "$NAMESPACE" 2>/dev/null \
  | grep "$CLAIM" || echo "    (none — clean!)"
echo "--- UyuniProvider (cluster-scoped) ---"
kubectl get uyuniprovider 2>/dev/null \
  | grep "$CLAIM" || echo "    (none)"
