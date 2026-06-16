#!/usr/bin/env bash
# Usage: ./cleanup-brandregion.sh <claim-name> <namespace>
#   e.g: ./cleanup-brandregion.sh delhaize-apac linux-platform
set -euo pipefail

CLAIM="${1:?Usage: $0 <claim-name> <namespace>}"
NAMESPACE="${2:?Usage: $0 <claim-name> <namespace>}"

echo "==> Deleting BrandRegionClaim $CLAIM in $NAMESPACE..."
kubectl delete brandregionclaim "$CLAIM" -n "$NAMESPACE" --ignore-not-found

echo "==> Waiting 5s for cascaded deletions to start..."
sleep 5

# Collect all resources that match the claim name prefix (BrandRegion adds a random suffix)
for kind in brandregion contentproject clmenvironment softwarechannel activationkey repository systemgroup; do
  resources=$(kubectl get "$kind" -n "$NAMESPACE" --no-headers 2>/dev/null \
    | awk '{print $1}' \
    | grep "^${CLAIM}" || true)

  if [ -z "$resources" ]; then
    continue
  fi

  echo "==> Force-removing finalizers from $kind..."
  for r in $resources; do
    kubectl patch "$kind/$r" -n "$NAMESPACE" \
      --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
  done

  echo "==> Deleting $kind resources..."
  # shellcheck disable=SC2086
  kubectl delete "$kind" $resources -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
done

echo ""
echo "==> Remaining resources matching '$CLAIM' in $NAMESPACE:"
kubectl get brandregionclaim,brandregion,contentproject,clmenvironment,softwarechannel,activationkey,repository,systemgroup \
  -n "$NAMESPACE" 2>/dev/null \
  | grep "$CLAIM" || echo "    (none — clean!)"
