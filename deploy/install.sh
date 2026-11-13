#!/usr/bin/env bash
# One-command install of Anton on a local kind cluster (mock-GPU mode).
#
#   deploy/install.sh              cluster + CloudNativePG + Anton
#   MONITORING=1 deploy/install.sh ...plus Prometheus + Grafana
#   SNAPSHOTS=1  deploy/install.sh ...plus CSI hostpath + VolumeSnapshots,
#                                  and sandboxes use PVC workspaces
#
# Needs docker, kind, kubectl and helm. Safe to re-run: it upgrades in place.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-anton}
DOCKER=${DOCKER:-docker}
KIND=${KIND:-kind}
TAG=${TAG:-dev}
log() { printf '\n== %s\n' "$*"; }

for tool in "$DOCKER" "$KIND" kubectl helm; do
  command -v "${tool%% *}" >/dev/null || { echo "missing: $tool" >&2; exit 1; }
done

if ! $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster $CLUSTER (1 control plane + 4 workers)"
  $KIND create cluster --name "$CLUSTER" --config deploy/kind.yaml
fi
# Point kubectl at the cluster (works when kind runs under sudo, too).
mkdir -p "$HOME/.kube"
$KIND get kubeconfig --name "$CLUSTER" > "$HOME/.kube/config"
chmod 600 "$HOME/.kube/config"

log "building images"
$DOCKER build -q -t "anton:$TAG" .
$DOCKER build -q -t "anton-sandbox:$TAG" --target sandbox .
$DOCKER build -q -t "anton-web:$TAG" -f Dockerfile.web .
$KIND load docker-image "anton:$TAG" "anton-sandbox:$TAG" "anton-web:$TAG" --name "$CLUSTER"

log "installing CloudNativePG"
helm repo add cnpg https://cloudnative-pg.github.io/charts >/dev/null 2>&1 || true
helm repo update cnpg >/dev/null
helm upgrade --install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace \
  ${CNPG_VERSION:+--version "$CNPG_VERSION"} --wait --timeout 5m >/dev/null
helm -n cnpg-system list -f '^cnpg$' -o json | sed -E 's/.*"chart":"([^"]*)".*/   chart: \1/'

EXTRA=()
if [ "${SNAPSHOTS:-0}" = "1" ]; then
  log "installing VolumeSnapshot CRDs, snapshot controller and CSI hostpath driver"
  deploy/storage/csi-hostpath.sh
  EXTRA+=(--set sandbox.workspace=pvc --set sandbox.storageClassName=csi-hostpath-sc
          --set sandbox.snapshotClassName=csi-hostpath-snapclass)
fi

if [ "${MONITORING:-0}" = "1" ]; then
  log "installing kube-prometheus-stack (Prometheus, Grafana)"
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
  helm repo update prometheus-community >/dev/null
  helm upgrade --install monitoring prometheus-community/kube-prometheus-stack -n monitoring --create-namespace \
    -f deploy/monitoring/values.yaml ${KPS_VERSION:+--version "$KPS_VERSION"} --wait --timeout 10m >/dev/null
  EXTRA+=(--set monitoring.enabled=true)
fi

log "installing Anton"
helm upgrade --install anton deploy/helm/anton -n anton --create-namespace \
  --set image.tag="$TAG" --set sandboxImage.tag="$TAG" --set webImage.tag="$TAG" "${EXTRA[@]}" ${HELM_ARGS:-} --wait --timeout 10m
kubectl -n anton rollout restart deployment >/dev/null   # pick up rebuilt :dev images
kubectl -n anton rollout status deployment --timeout=5m

log "done"
echo "Web UI:  kubectl -n anton port-forward svc/anton-api 8000:8000   then open http://localhost:8000"
[ "${MONITORING:-0}" = "1" ] && echo "Grafana: kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80   (admin / see deploy/monitoring/values.yaml)"
echo "Bench:   kubectl -n anton create -f deploy/bench/job.yaml && kubectl -n anton logs -f job/anton-bench"
