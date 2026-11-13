#!/usr/bin/env bash
# Checks multi-tenancy on a running install (docs/multitenancy.md). Run where
# kubectl points at the cluster, after:
#   helm upgrade anton deploy/helm/anton -n anton --reuse-values --set tenancy.enabled=true
#
# It creates one session for each of two teams through the API and verifies:
# each team gets its own provisioned namespace, a chat turn works, platform
# service accounts can reach team namespaces and nothing else, and one team's
# sandbox cannot reach the other's. Prints PASS/FAIL per check; exits 1 on any FAIL.
set -uo pipefail
A=${TEAM_A:-alpha}; B=${TEAM_B:-beta}
NSA=anton-team-$A; NSB=anton-team-$B
fail=0
ok()   { echo "PASS  $*"; }
bad()  { echo "FAIL  $*"; fail=1; }
check() { local what=$1; shift; if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi; }
deny()  { local what=$1; shift; if "$@" >/dev/null 2>&1; then bad "$what"; else ok "$what"; fi; }
can()   { kubectl auth can-i "$@" 2>/dev/null | grep -qx yes; }

kubectl -n anton rollout status deploy/anton-operator deploy/anton-router deploy/anton-api deploy/anton-sync --timeout=5m >/dev/null \
  || bad "platform deployments rolled out with tenancy on"

PORT=18001
kubectl -n anton port-forward svc/anton-api $PORT:8000 >/dev/null 2>&1 & PF=$!
trap 'kill $PF 2>/dev/null' EXIT

# One session per team through the API, then one chat turn each.
OUT=$(python3 - "$PORT" "$A" "$B" 2>&1 <<'PY'
import json, sys, time, urllib.error, urllib.request
port, teams = sys.argv[1], sys.argv[2:]
def call(method, path, body=None, token=None, timeout=300):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json", **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise SystemExit(f"HTTP {e.code} from {method} {path}: {e.read().decode(errors='replace')[:400]}")
for _ in range(60):
    try:
        call("GET", "/healthz"); break
    except OSError:
        time.sleep(1)
for team in teams:
    tok = call("POST", "/api/login", {"name": f"{team}-user", "team": team})["token"]
    name = call("POST", "/api/sessions", {"label": "tenancy", "template": "agent"}, tok)["name"]
    out = None
    for attempt in range(6):  # the first message waits for the sandbox; 504 = not ready yet
        try:
            out = call("POST", f"/api/sessions/{name}/messages", {"content": "say hello"}, tok)
            break
        except SystemExit as e:
            if "HTTP 504" not in str(e) or attempt == 5:
                raise
    print(f"SESSION {team} {name} reply={'yes' if out and out.get('reply') else 'no'}")
PY
)
echo "$OUT" | sed 's/^/      /'
SA=$(echo "$OUT" | awk -v t="$A" '$1=="SESSION" && $2==t {print $3}')
SB=$(echo "$OUT" | awk -v t="$B" '$1=="SESSION" && $2==t {print $3}')
[ -n "$SA" ] && [ -n "$SB" ] && ok "the API created a session for each team" || bad "the API created a session for each team"
echo "$OUT" | grep -c "reply=yes" | grep -qx 2 && ok "a chat turn worked for both teams" || bad "a chat turn worked for both teams"

for pair in "$NSA:$SA" "$NSB:$SB"; do
  ns=${pair%%:*}; s=${pair##*:}
  check "$ns exists with the tenant label" sh -c "kubectl get ns $ns -o jsonpath='{.metadata.labels.anton\.dev/tenant}' | grep -qx true"
  check "$ns holds the session $s" kubectl -n "$ns" get agentsession "$s"
  check "$ns holds its sandbox pod, Ready" kubectl -n "$ns" wait --for=condition=Ready "pod/$s-sandbox" --timeout=120s
  deny  "the session $s is not in the platform namespace" kubectl -n anton get agentsession "$s"
  check "$ns has the operator, API and router RoleBindings" sh -c "kubectl -n $ns get rolebinding anton-tenant-operator-anton-operator anton-tenant-api-anton-api anton-tenant-router-anton-router"
  check "$ns has the default-deny sandbox NetworkPolicy" kubectl -n "$ns" get networkpolicy anton-sandbox
  check "$ns has its ResourceQuota and LimitRange" sh -c "kubectl -n $ns get resourcequota anton-team && kubectl -n $ns get limitrange anton-sandbox"
done

# Least privilege, by impersonating the platform service accounts.
op=system:serviceaccount:anton:anton-operator; api=system:serviceaccount:anton:anton-api; rt=system:serviceaccount:anton:anton-router
check "operator may manage pods in a team namespace"            can create pods -n "$NSA" --as=$op
deny  "operator may NOT list pods cluster-wide"                 can list pods --all-namespaces --as=$op
deny  "operator may NOT touch pods in kube-system"              can create pods -n kube-system --as=$op
check "API may create sessions in a team namespace"             can create agentsessions.anton.dev -n "$NSA" --as=$api
deny  "API may NOT read secrets in a team namespace"            can get secrets -n "$NSA" --as=$api
deny  "API may NOT create pods in kube-system"                  can create pods -n kube-system --as=$api
check "router may patch session status in a team namespace"     can patch agentsessions.anton.dev/status -n "$NSA" --as=$rt
deny  "router may NOT read pods in a team namespace"            can get pods -n "$NSA" --as=$rt
deny  "API may NOT create a namespace outside anton-team-*"     kubectl create namespace anton-check-not-a-team --as=$api
kubectl delete namespace anton-check-not-a-team --ignore-not-found >/dev/null 2>&1

# Network isolation between teams (needs a CNI that enforces NetworkPolicy).
IPB=$(kubectl -n "$NSB" get pod "$SB-sandbox" -o jsonpath='{.status.podIP}' 2>/dev/null)
if [ -n "$IPB" ] && [ -n "$SA" ]; then
  check "team $A's sandbox reaches the router"             kubectl -n "$NSA" exec "$SA-sandbox" -- wget -T 5 -qO- http://anton-router.anton.svc:8080/healthz
  deny  "team $A's sandbox canNOT reach team $B's sandbox" kubectl -n "$NSA" exec "$SA-sandbox" -- wget -T 5 -qO- "http://$IPB:8080/healthz"
else
  bad "network isolation check (no sandbox IP)"
fi

echo; [ "$fail" = 0 ] && echo "TENANCY CHECK PASSED" || echo "TENANCY CHECK FAILED"
exit $fail
