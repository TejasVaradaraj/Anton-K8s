#!/usr/bin/env bash
# Runs ON the cluster host: applies a benchmark profile's measured rates to
# the running install, runs the benchmark with the profile's workload, and
# saves the logs in results/.
#   deploy/bench/run-profile.sh qwen7b-l4
#   deploy/bench/run-profile.sh qwen7b-l4 --seeds=1 --duration=1m --modes=anton   (extra args override)
set -euo pipefail
cd "$(dirname "$0")/../.."
P=$1; shift
D=deploy/bench/profiles/$P
[ -d "$D" ] || { echo "no profile $P" >&2; exit 1; }

# The profile's values.yaml sets sandbox.agent; keep the agent the caller
# installed (deploy/eks/run.sh switches between builtin and opencode).
AGENT=$(helm get values anton -n anton --all -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox"]["agent"])')
helm upgrade anton deploy/helm/anton -n anton --reuse-values -f "$D/values.yaml" --set sandbox.agent="$AGENT" --wait --timeout 30m >/dev/null
echo "bench: sandbox agent $AGENT"
kubectl -n anton rollout status deployment --timeout=30m >/dev/null

args=$( { grep -v -E '^\s*(#|$)' "$D/args"; for a in "$@"; do echo "$a"; done; } | sed 's/"/\\"/g; s/^/"/; s/$/"/' | paste -sd, -)
kubectl -n anton delete job anton-bench --ignore-not-found --wait >/dev/null
IMAGE=$(kubectl -n anton get deploy anton-router -o jsonpath='{.spec.template.spec.containers[0].image}')
# kind: the bench runs on the control-plane node. Managed clusters (EKS) have
# none; there it runs on the system node, which is never disrupted.
if [ -z "$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o name)" ]; then
  PLACE='"nodeSelector": {"anton.dev/role": "system"}, "tolerations": [],'
else
  PLACE='"nodeSelector": {"node-role.kubernetes.io/control-plane": ""}, "tolerations": [{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"}],'
fi
sed "s|__ARGS__|$args|; s|__PROFILE__|$P|; s|__IMAGE__|$IMAGE|; s|__PLACEMENT__|$PLACE|" deploy/bench/job.template.json | kubectl -n anton create -f -
kubectl -n anton wait --for=condition=Ready pod -l app=anton-bench --timeout=180s >/dev/null

mkdir -p results
out="results/bench-$P-$(date +%Y%m%d-%H%M%S).log"
kubectl -n anton logs -f job/anton-bench > "$out"
kubectl -n anton logs deployment/anton-operator --tail=5000 > "${out%.log}-operator.log" 2>&1 || true
echo "saved $out"
grep -E '^\| ' "$out" | tail -25
