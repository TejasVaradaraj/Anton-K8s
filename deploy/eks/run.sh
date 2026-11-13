#!/usr/bin/env bash
# One scripted real-GPU session on AWS EKS: 2x g6.xlarge spot (the account's
# spot quota is 8 vCPUs). Runs on the AWS bench box, which needs AWS
# credentials (an instance role) and has docker, helm, kubectl, terraform.
#
#   deploy/eks/run.sh
#
# Stages, cheapest first, so a mistake fails before any GPU is paid for:
#   1. spot price + quota check; cluster with one CPU node, ECR; push images
#   2. install Anton (vLLM backend, OpenCode sandboxes); the LMCache server
#      must start on the CPU node; replicas wait for GPUs
#   3. GPU NodePool (Karpenter launches 2 spot L4s) and wait for vLLM
#   4. smoke: an OpenCode session answers through the router
#   5. recovery, sequential (drain one GPU node, bring it back, kill the
#      other): builtin agent (KV warmth from vLLM's cached_tokens), then OpenCode
#   6. a REAL spot interruption (AWS FIS, 2-minute notice) during agent load
#   7. save results; destroy everything (always, via trap)
# MAX_MINUTES (default 300) and MAX_SPOT_PRICE (default 0.60 $/h per GPU)
# bound the spend.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
MAX_MINUTES=${MAX_MINUTES:-300}
MAX_SPOT_PRICE=${MAX_SPOT_PRICE:-0.60}
REGION=${AWS_REGION:-us-east-2}
export AWS_DEFAULT_REGION=$REGION
# Not from `terraform output`: before the first apply it prints a warning box
# on stdout, which would become the zone name.
ZONE=${WORKLOAD_ZONE:-us-east-2c}
# GPU_TYPE=g4dn.xlarge PROFILE=eks-t4 runs on T4s (4-bit model) when L4 spot has no capacity.
GPU_TYPE=${GPU_TYPE:-g6.xlarge}
PROFILE=${PROFILE:-eks-l4}
[ -d "$REPO/deploy/bench/profiles/$PROFILE" ] || { echo "no profile $PROFILE"; exit 1; }
OUT=$REPO/results/eks-$(date -u +%Y%m%d-%H%M%S)
mkdir -p "$OUT"
log() { printf '\n== %s  %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
DOCKER=${DOCKER:-docker}; $DOCKER ps >/dev/null 2>&1 || DOCKER="sudo docker"
# A private kubeconfig: never switch the box's default (kind) context.
export KUBECONFIG=$OUT/kubeconfig

destroy() {
  log "destroying everything"
  if [ -s "$KUBECONFIG" ]; then   # what the cluster looked like, for a failed run
    { kubectl get nodes -o wide; kubectl get pods -A -o wide; kubectl -n anton describe pods
      kubectl get events -A --sort-by=.lastTimestamp; } > "$OUT/final-state.log" 2>&1 || true
    # Pod logs die with the cluster; a failed stage is undiagnosable without them.
    mkdir -p "$OUT/pod-logs"
    for p in $(kubectl -n anton get pods -o name 2>/dev/null); do
      timeout 30 kubectl -n anton logs "$p" --all-containers --tail=3000 > "$OUT/pod-logs/$(basename "$p").log" 2>&1 || true
      timeout 30 kubectl -n anton logs "$p" --all-containers --previous --tail=1000 > "$OUT/pod-logs/$(basename "$p").previous.log" 2>/dev/null || true
    done
    kubectl -n anton get agentsessions -o yaml > "$OUT/sessions.yaml" 2>&1 || true
  fi
  # Stop the workloads first: a replica left Pending while its node drains makes
  # Karpenter launch a replacement during teardown.
  if [ -s "$KUBECONFIG" ]; then
    kubectl -n anton delete agentsessions --all --wait=false >>"$OUT/teardown.log" 2>&1 || true
    kubectl -n anton scale deploy/anton-replica --replicas=0 >>"$OUT/teardown.log" 2>&1 || true
  fi
  if kubectl get nodepool gpu >/dev/null 2>&1; then
    kubectl delete nodepool gpu --wait=true --timeout=10m >>"$OUT/teardown.log" 2>&1 || true
    kubectl delete ec2nodeclass gpu --wait=true --timeout=5m >>"$OUT/teardown.log" 2>&1 || true
  fi
  # GPU instances Karpenter did not finish removing go BEFORE Terraform: on
  # 2026-11-13 one T4 outlived the cluster by an hour, and it blocked the
  # subnet, gateway and security group, so both destroys failed.
  LEFT=$(aws ec2 describe-instances --filters Name=tag:karpenter.sh/nodepool,Values=gpu \
    Name=instance-state-name,Values=pending,running,stopping --query 'Reservations[].Instances[].InstanceId' --output text)
  if [ -n "$LEFT" ]; then
    aws ec2 terminate-instances --instance-ids $LEFT >/dev/null && log "terminated leftover GPU instances: $LEFT"
    timeout 600 aws ec2 wait instance-terminated --instance-ids $LEFT || true
  fi
  terraform destroy -auto-approve -input=false -var max_gpus=2 -var "workload_zone=$ZONE" >>"$OUT/teardown.log" 2>&1 \
    || terraform destroy -auto-approve -input=false -var max_gpus=2 -var "workload_zone=$ZONE" >>"$OUT/teardown.log" 2>&1 \
    || log "WARNING: terraform destroy failed (see teardown.log)"
  LEFT=$(aws ec2 describe-instances --filters Name=tag:karpenter.sh/nodepool,Values=gpu \
    Name=instance-state-name,Values=pending,running,stopping --query 'Reservations[].Instances[].InstanceId' --output text)
  [ -n "$LEFT" ] && aws ec2 terminate-instances --instance-ids $LEFT >/dev/null && log "terminated leftover GPU instances: $LEFT"
  [ "$(terraform state list 2>/dev/null | grep -vc '^data\.')" = "0" ] || log "WARNING: Terraform still tracks resources; run deploy/eks/down.sh"
  # Workspace and Postgres volumes outlive the cluster: the EBS CSI driver is
  # destroyed before it can delete them (11 were left on 2026-11-13).
  for v in $(aws ec2 describe-volumes --filters Name=status,Values=available Name=tag:ebs.csi.aws.com/cluster,Values=true \
      --query 'Volumes[?starts_with(Tags[?Key==`Name`]|[0].Value, `anton-dynamic-pvc-`)].VolumeId' --output text); do
    aws ec2 delete-volume --volume-id "$v" >/dev/null 2>&1 && echo "deleted leftover volume $v" >>"$OUT/teardown.log"
  done
  CL=$(aws eks list-clusters --query 'clusters' --output text)
  echo "$CL" | grep -qw anton && log "WARNING: EKS cluster anton still present" || log "nothing left running"
  # The time-limit watchdog must not outlive the run (it kept pgrep matching after an early failure).
  [ -n "${WATCHDOG:-}" ] && pkill -P "$WATCHDOG" 2>/dev/null; kill "${WATCHDOG:-}" 2>/dev/null || true
  [ -n "${FIS_WAITER:-}" ] && kill "$FIS_WAITER" 2>/dev/null || true
}
trap destroy EXIT
( sleep $((MAX_MINUTES * 60)); log "time limit reached"; kill -TERM $$ ) & WATCHDOG=$!

log "1/7 checks: account, spot quota, spot price"
aws sts get-caller-identity --query Account --output text | tee -a "$OUT/run.log"
QUOTA=$(aws service-quotas get-service-quota --service-code ec2 --quota-code L-3819A6DF --query Quota.Value --output text)
[ "${QUOTA%.*}" -ge 8 ] || { log "spot GPU quota is $QUOTA vCPUs; need 8"; exit 1; }
# Without this role every spot launch fails (AuthFailure.ServiceLinkedRoleCreationNotPermitted);
# Karpenter's role may not create it. Created once per account.
aws iam get-role --role-name AWSServiceRoleForEC2Spot >/dev/null 2>&1 \
  || { log "missing AWSServiceRoleForEC2Spot: run 'aws iam create-service-linked-role --aws-service-name spot.amazonaws.com'"; exit 1; }
PRICE=$(aws ec2 describe-spot-price-history --instance-types "$GPU_TYPE" --product-descriptions Linux/UNIX \
  --availability-zone "$ZONE" --start-time "$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%S)" \
  --query 'SpotPriceHistory[0].SpotPrice' --output text)
log "$GPU_TYPE spot in $ZONE (profile $PROFILE): \$$PRICE/h (limit \$$MAX_SPOT_PRICE)"
python3 -c "import sys; sys.exit(0 if float('$PRICE') <= float('$MAX_SPOT_PRICE') else 1)" || { log "spot price above the limit"; exit 1; }

log "preflight: every workload fits a node this cluster will have"
python3 preflight.py anton ../helm/anton -n anton -f "../bench/profiles/$PROFILE/values.yaml" \
  --set image.repository=x --set sandboxImage.repository=x --set webImage.repository=x | tee "$OUT/preflight.log" | tail -1
grep -q "PREFLIGHT OK" "$OUT/preflight.log" || { log "preflight failed (see preflight.log)"; exit 1; }

log "cluster (CPU node only), ECR"
terraform init -upgrade -input=false >"$OUT/terraform.log" 2>&1
terraform apply -auto-approve -input=false -var max_gpus=2 -var "workload_zone=$ZONE" >>"$OUT/terraform.log" 2>&1
aws eks update-kubeconfig --name "$(terraform output -raw cluster_name)" --kubeconfig "$KUBECONFIG" >/dev/null
ECR=$(terraform output -raw ecr_registry)
aws ecr get-login-password | $DOCKER login --username AWS --password-stdin "$ECR" >/dev/null
for img in anton anton-sandbox anton-web; do
  $DOCKER tag "$img:dev" "$ECR/$img:run" && $DOCKER push -q "$ECR/$img:run" >/dev/null
done
kubectl apply -f storage.yaml >/dev/null

log "2/7 install CloudNativePG and Anton (replicas wait for GPUs)"
helm repo add cnpg https://cloudnative-pg.github.io/charts >/dev/null 2>&1 || true
helm upgrade --install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace \
  --set nodeSelector."anton\.dev/role"=system --wait --timeout 5m >/dev/null
helm upgrade --install anton ../helm/anton -n anton --create-namespace \
  -f "../bench/profiles/$PROFILE/values.yaml" \
  --set image.repository="$ECR/anton" --set image.tag=run --set image.pullPolicy=Always \
  --set sandboxImage.repository="$ECR/anton-sandbox" --set sandboxImage.tag=run \
  --set webImage.repository="$ECR/anton-web" --set webImage.tag=run >/dev/null
kubectl -n anton rollout status deploy/anton-lmcache --timeout=25m    # 9 GB image on the CPU node
kubectl -n anton logs deploy/anton-lmcache --tail=20 | tee "$OUT/lmcache-server.log"
for d in anton-operator anton-router anton-api anton-tier anton-sync; do kubectl -n anton rollout status deploy/$d --timeout=10m; done

log "3/7 GPU NodePool: Karpenter launches 2 spot GPU nodes (spend starts)"
sed -e "s/__NAME__/$(terraform output -raw cluster_name)/" \
    -e "s/__NODE_ROLE__/$(terraform output -raw karpenter_node_role)/" \
    -e "s/__MAX_GPUS__/2/" -e "s/__ZONE__/$ZONE/" -e "s/__GPU_TYPE__/$GPU_TYPE/" gpu-nodepool.yaml | kubectl apply -f - >/dev/null
kubectl -n anton rollout status deploy/anton-replica --timeout=45m
kubectl get nodes -L node.kubernetes.io/instance-type,karpenter.sh/capacity-type,topology.kubernetes.io/zone | tee -a "$OUT/run.log"

# HOLD_MINUTES>0: when the smoke test fails, keep the cluster and GPUs up to
# that long instead of destroying them, so a fix can be tried on the live
# cluster (a new cluster costs 25 minutes). `touch $OUT/retry` re-runs the
# smoke test, `touch $OUT/abort` gives up. MAX_MINUTES still bounds the run.
hold() {
  [ "${HOLD_MINUTES:-0}" -gt 0 ] || return 1
  log "HOLD: $1 failed; waiting up to ${HOLD_MINUTES}m for $OUT/retry or $OUT/abort"
  local end=$((SECONDS + HOLD_MINUTES * 60))
  while [ "$SECONDS" -lt "$end" ]; do
    [ -e "$OUT/abort" ] && { rm -f "$OUT/abort"; log "HOLD aborted"; return 1; }
    [ -e "$OUT/retry" ] && { rm -f "$OUT/retry"; log "HOLD over: retrying $1"; return 0; }
    sleep 5
  done
  log "HOLD expired"; return 1
}

log "4/7 smoke: an OpenCode session through the router"
until python3 "$REPO/deploy/doks/smoke.py" 2>&1 | tee "$OUT/smoke.log"; do
  cp "$OUT/smoke.log" "$OUT/smoke-failed-$(date -u +%H%M%S).log"
  hold smoke || exit 1
done

BENCH="$REPO/deploy/bench/run-profile.sh $PROFILE --replica-timeout=25m"
# SKIP_RECOVERY=1 skips stages 5 and 5b (already measured on 2026-11-13) to re-run only the spot stage.
if [ "${SKIP_RECOVERY:-0}" != "1" ]; then
log "5/7 recovery, builtin agent (KV warmth from vLLM's own cached_tokens)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin --wait --timeout 30m >/dev/null
$BENCH --scenario=recovery --recovery-sequential --modes="${STAGE5_MODES:-plain,sandbox,routing,anton}" --recovery-turns=3 \
  2>&1 | tee "$OUT/recovery-builtin.log" || true
if [ "${SKIP_OPENCODE_RECOVERY:-0}" != "1" ]; then
log "5b/7 recovery, OpenCode (a real coding agent's conversation survives)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=opencode --wait --timeout 30m >/dev/null
$BENCH --scenario=recovery --recovery-sequential --modes=anton --sessions=4 --recovery-turns=2 \
  2>&1 | tee "$OUT/recovery-opencode.log" || true
fi
fi

log "6/7 real spot interruption (AWS FIS) during real agent conversations, anton mode"
# The recovery scenario, not the load one: the load scenario sends the
# simulator's shorthand requests, which real vLLM rejects (708 of 708 errors on
# 2026-11-13). FIS fires only once the conversations exist and the benchmark
# says it is waiting.
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin --wait --timeout 30m >/dev/null
( for _ in $(seq 1 360); do
    if kubectl -n anton logs job/anton-bench --tail=300 2>/dev/null | grep -q "waiting for an external disruption"; then
      log "sending a real spot interruption to one GPU node"
      aws fis start-experiment --experiment-template-id "$(terraform output -raw fis_spot_template)" \
        --query 'experiment.id' --output text >>"$OUT/run.log" 2>&1
      exit 0
    fi
    sleep 5
  done
  log "the benchmark never asked for the interruption; none sent" ) & FIS_WAITER=$!
$BENCH --scenario=recovery --chaos=external --modes=anton --recovery-turns=3 2>&1 | tee "$OUT/spot-real.log" || true
kill "$FIS_WAITER" 2>/dev/null || true

# Baseline for the real-GPU numbers: the same drain and hard kill with the
# shared LMCache tier switched off and sessions pinned to one replica
# (routing mode). In stage 5 every mode kept its caches because LMCache writes
# through to the shared server; this shows what is lost without it.
if [ "${BASELINE_NO_TIER:-1}" = "1" ]; then
  log "6b/7 baseline: recovery with the shared KV tier off (routing mode)"
  helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin \
    --set inference.vllm.lmcache.remote=false --wait --timeout 30m >/dev/null || true
  # run-profile.sh upgrades with --reuse-values, so remote=false stays set.
  kubectl -n anton get configmap anton-lmcache -o jsonpath='{.data.lmcache\.yaml}' | tee "$OUT/lmcache-no-tier.yaml"
  $BENCH --scenario=recovery --recovery-sequential --modes=routing --recovery-turns=3 \
    2>&1 | tee "$OUT/recovery-no-tier.log" || true
  kubectl -n anton get configmap anton-lmcache -o jsonpath='{.data.lmcache\.yaml}' | grep -q remote_url \
    && log "WARNING: the shared tier was back on during the baseline; discard recovery-no-tier.log"
fi

log "7/7 saving logs"
kubectl -n anton logs deploy/anton-operator --tail=5000 > "$OUT/operator.log" 2>&1 || true
kubectl -n anton logs deploy/anton-router --tail=5000 > "$OUT/router.log" 2>&1 || true
kubectl get events -A --sort-by=.lastTimestamp > "$OUT/events.log" 2>&1 || true
kill $WATCHDOG 2>/dev/null || true
log "done; results in $OUT"
