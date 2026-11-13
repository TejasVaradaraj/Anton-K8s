#!/usr/bin/env bash
# One scripted real-GPU session on DigitalOcean. Runs on the AWS bench box
# (it has docker, helm, kubectl, terraform; this script installs doctl).
#
#   DO_TOKEN_FILE=~/.do-token deploy/doks/run.sh
#
# Stages, cheapest first, so a mistake fails before any GPU is paid for:
#   1. cluster with one CPU node + container registry; push images
#   2. install Anton (vLLM backend, OpenCode sandboxes); the LMCache server
#      must start on the CPU node; replicas wait for GPUs
#   3. add 2 GPU nodes (spend starts) and wait for vLLM to load the model
#   4. smoke: an OpenCode session answers through the router, twice
#   5. recovery scenario (drain one GPU node, bring it back, kill the other)
#   6. short real-GPU load benchmark
#   7. save results; destroy everything (always, via trap)
# A hard limit (MAX_MINUTES, default 240) bounds the spend.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
MAX_MINUTES=${MAX_MINUTES:-240}
REGION=${REGION:-tor1}
OUT=$REPO/results/doks-$(date -u +%Y%m%d-%H%M%S)
mkdir -p "$OUT"
log() { printf '\n== %s  %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }

: "${DO_TOKEN_FILE:?set DO_TOKEN_FILE to a file holding the DigitalOcean API token}"
export DIGITALOCEAN_TOKEN=$(tr -d '[:space:]' < "$DO_TOKEN_FILE")
export DIGITALOCEAN_ACCESS_TOKEN=$DIGITALOCEAN_TOKEN

if ! command -v doctl >/dev/null; then
  curl -fsSL -o /tmp/doctl.tgz https://github.com/digitalocean/doctl/releases/download/v1.146.0/doctl-1.146.0-linux-amd64.tar.gz
  tar -xzf /tmp/doctl.tgz -C /tmp && sudo install /tmp/doctl /usr/local/bin/doctl
fi
DOCKER=${DOCKER:-docker}; $DOCKER ps >/dev/null 2>&1 || DOCKER="sudo docker"

destroy() {
  log "destroying everything"
  terraform destroy -auto-approve -var "region=$REGION" -var gpu_nodes=0 >>"$OUT/terraform.log" 2>&1 \
    || terraform destroy -auto-approve -var "region=$REGION" -var gpu_nodes=0 >>"$OUT/terraform.log" 2>&1
  LEFT=$(doctl kubernetes cluster list --format Name --no-header 2>/dev/null | grep -x anton || true)
  [ -z "$LEFT" ] && log "nothing left running" || log "WARNING: cluster still present, delete it in the DO console"
}
trap destroy EXIT
# Hard spend limit: kill this script (which runs destroy) after MAX_MINUTES.
( sleep $((MAX_MINUTES * 60)); log "time limit reached"; kill -TERM $$ ) & WATCHDOG=$!

log "1/7 cluster (CPU node only) and registry in $REGION"
terraform init -input=false >"$OUT/terraform.log" 2>&1
terraform apply -auto-approve -input=false -var "region=$REGION" -var gpu_nodes=0 >>"$OUT/terraform.log" 2>&1
# A private kubeconfig: never switch the box's default context (the kind
# cluster may be busy with another run).
export KUBECONFIG=$OUT/kubeconfig
doctl kubernetes cluster kubeconfig show "$(terraform output -raw cluster_id)" > "$KUBECONFIG"
chmod 600 "$KUBECONFIG"
REG=$(terraform output -raw registry)
doctl registry login >/dev/null
for img in anton anton-sandbox anton-web; do
  $DOCKER tag "$img:dev" "$REG/$img:run" && $DOCKER push -q "$REG/$img:run" >/dev/null
done
# DOKS registry integration: pull secret in every namespace on the default
# service accounts (used by sandboxes and replicas).
doctl kubernetes cluster registry add "$(terraform output -raw cluster_id)" >/dev/null

log "2/7 install CloudNativePG and Anton (replicas wait for GPUs)"
helm repo add cnpg https://cloudnative-pg.github.io/charts >/dev/null 2>&1 || true
helm upgrade --install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace --wait --timeout 5m >/dev/null
kubectl get volumesnapshotclass -o name | tee -a "$OUT/run.log"
kubectl create namespace anton --dry-run=client -o yaml | kubectl apply -f - >/dev/null
SECRET=$(doctl registry kubernetes-manifest --namespace anton -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["name"])')
doctl registry kubernetes-manifest --namespace anton | kubectl apply -f - >/dev/null
helm upgrade --install anton ../helm/anton -n anton \
  -f ../bench/profiles/doks-rtx4000/values.yaml \
  --set image.repository="$REG/anton" --set image.tag=run --set image.pullPolicy=Always \
  --set sandboxImage.repository="$REG/anton-sandbox" --set sandboxImage.tag=run \
  --set webImage.repository="$REG/anton-web" --set webImage.tag=run \
  --set-json "imagePullSecrets=[{\"name\":\"$SECRET\"}]" >/dev/null
kubectl -n anton rollout status deploy/anton-lmcache --timeout=20m   # 9 GB image on the CPU node
kubectl -n anton logs deploy/anton-lmcache --tail=20 | tee "$OUT/lmcache-server.log"
for d in anton-operator anton-router anton-api anton-tier; do kubectl -n anton rollout status deploy/$d --timeout=10m; done

log "3/7 add 2 GPU nodes (spend starts) and wait for vLLM"
terraform apply -auto-approve -input=false -var "region=$REGION" -var gpu_nodes=2 >>"$OUT/terraform.log" 2>&1
kubectl -n anton rollout status deploy/anton-replica --timeout=40m
kubectl -n anton get pods -o wide | tee -a "$OUT/run.log"

log "4/7 smoke: an OpenCode session through the router"
python3 "$REPO/deploy/doks/smoke.py" | tee "$OUT/smoke.log"

log "5/7 recovery (builtin agent: measures KV warmth from vLLM's own cached_tokens)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin --wait >/dev/null
kubectl -n anton rollout status deploy/anton-operator --timeout=5m
"$REPO/deploy/bench/run-profile.sh" doks-rtx4000 --scenario=recovery --recovery-sequential \
  --modes=plain,sandbox,routing,anton --recovery-turns=3 2>&1 | tee "$OUT/recovery-builtin.log" || true

log "5b/7 recovery (OpenCode: a real coding agent's conversation survives)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=opencode --wait >/dev/null
kubectl -n anton rollout status deploy/anton-operator --timeout=5m
"$REPO/deploy/bench/run-profile.sh" doks-rtx4000 --scenario=recovery --recovery-sequential \
  --modes=anton --sessions=4 --recovery-turns=2 2>&1 | tee "$OUT/recovery-opencode.log" || true

log "6/7 short real-GPU load benchmark"
"$REPO/deploy/bench/run-profile.sh" doks-rtx4000 --modes=routing,anton --seeds=1 --duration=3m 2>&1 | tee "$OUT/load.log" || true

log "7/7 saving logs"
kubectl -n anton logs deploy/anton-operator --tail=5000 > "$OUT/operator.log" 2>&1 || true
kubectl -n anton logs deploy/anton-router --tail=5000 > "$OUT/router.log" 2>&1 || true
kill $WATCHDOG 2>/dev/null || true
log "done; results in $OUT"
