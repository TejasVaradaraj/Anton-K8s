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
  CL=$(aws eks list-clusters --query 'clusters' --output text)
  echo "$CL" | grep -qw anton && log "WARNING: EKS cluster anton still present" || log "nothing left running"
  # The time-limit watchdog must not outlive the run (it kept pgrep matching after an early failure).
  [ -n "${WATCHDOG:-}" ] && pkill -P "$WATCHDOG" 2>/dev/null; kill "${WATCHDOG:-}" 2>/dev/null || true
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

log "4/7 smoke: an OpenCode session through the router"
python3 "$REPO/deploy/doks/smoke.py" | tee "$OUT/smoke.log"

BENCH="$REPO/deploy/bench/run-profile.sh $PROFILE --replica-timeout=25m"
log "5/7 recovery, builtin agent (KV warmth from vLLM's own cached_tokens)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin --wait --timeout 30m >/dev/null
$BENCH --scenario=recovery --recovery-sequential --modes=plain,sandbox,routing,anton --recovery-turns=3 \
  2>&1 | tee "$OUT/recovery-builtin.log" || true
log "5b/7 recovery, OpenCode (a real coding agent's conversation survives)"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=opencode --wait --timeout 30m >/dev/null
$BENCH --scenario=recovery --recovery-sequential --modes=anton --sessions=4 --recovery-turns=2 \
  2>&1 | tee "$OUT/recovery-opencode.log" || true

log "6/7 real spot interruption (AWS FIS) during builtin-agent load, anton mode"
helm upgrade anton ../helm/anton -n anton --reuse-values --set sandbox.agent=builtin --wait --timeout 30m >/dev/null
( sleep 240; log "sending a real spot interruption to one GPU node"
  aws fis start-experiment --experiment-template-id "$(terraform output -raw fis_spot_template)" \
    --query 'experiment.id' --output text >>"$OUT/run.log" 2>&1 ) &
$BENCH --chaos=external --modes=anton --duration=8m --hibernate=0 2>&1 | tee "$OUT/spot-real.log" || true

log "7/7 saving logs"
kubectl -n anton logs deploy/anton-operator --tail=5000 > "$OUT/operator.log" 2>&1 || true
kubectl -n anton logs deploy/anton-router --tail=5000 > "$OUT/router.log" 2>&1 || true
kubectl get events -A --sort-by=.lastTimestamp > "$OUT/events.log" 2>&1 || true
kill $WATCHDOG 2>/dev/null || true
log "done; results in $OUT"
