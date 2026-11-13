# Anton Platform

A Kubernetes platform for AI coding agents whose sandbox and LLM context survive node drains, spot interruptions, scale-downs and scale-to-zero.

Kubernetes treats pods as disposable. A long-running agent session holds state in two places that this destroys: its **sandbox** (workspace files) and the model's **KV cache** (the already-computed context on an inference replica). Lose the KV cache and the model must re-read the whole conversation before its next token.

Anton ties both to one resource, `AgentSession`, and manages them through disruption:

- An **operator** hibernates and wakes sessions, and a **disruption guard** rescues KV caches from replicas that are about to die, choosing which to save when the deadline is too short for all of them.
- A **session router** sends each session to the replica holding its cache, breaks that affinity only when queueing costs more than recomputing, and restores a session's KV cache before its first token after a move or a wake.
- A **product layer** (FastAPI, Postgres, Next.js) lets a team launch sessions, chat with agents, watch the lifecycle timeline, see the cluster, and run the chaos benchmark.

## Results

Three kinds of numbers, kept apart: **measurements** of the real serving stack on real GPUs (section 1), a **simulated chaos benchmark** whose replicas replay those measurements (section 2), and a **real-GPU run on EKS** with real vLLM, LMCache and coding agents (section 3). Read the Limitations section before quoting any of them. The raw logs named below live in `results/`, which is kept out of this repository.

### 1. Measured: when does keeping a KV cache pay?

`bench/vllm_lmcache_calibrate.ipynb`, vLLM 0.33 + LMCache 0.5.6 (CPU backend) on Colab GPUs. Raw data in `bench/calibration_vllm_*.json`. "Move" is offloading a session's cache to the tier and restoring it later; Anton moves a cache only when that is cheaper than recomputing it.

| Model, GPU | Prefill at 16k tokens | Offload + restore at 16k | Move ÷ recompute | Verdict |
|---|---|---|---|---|
| Qwen2.5-1.5B, L4 | 1.31 s | 0.64 + 1.02 s | 1.27 | recompute |
| Qwen2.5-7B, L4 | 4.83 s | 0.72 + 1.46 s | **0.45** | move |
| Qwen2.5-7B, A100 40 GB | 1.17 s | 0.73 + 1.41 s | 1.83 | recompute |
| Qwen2.5-7B, L4, LMCache chunk 2048 | 4.95 s | 0.46 + 1.37 s | **0.37** | move |

- **Moving pays when prefill is slow relative to the cache tier.** The 7B model on an L4 recomputes 16k tokens in 4.8 s but restores them in 1.5 s, so Anton keeps caches. A 1.5B model recomputes too cheaply for that, and an A100 recomputes 4× faster than an L4 while the tier restores no faster (about 0.65 GB/s on both GPUs), so recomputing wins there. The cost model has to be measured per deployment; Anton's rule (`internal/planner`) uses these measured curves.
- **Prefill grows faster than linearly** (7B on L4: 0.25 ms/token at 1k, 0.37 ms/token at 32k), so Anton prices recompute from the measured curve, not a single rate.
- **The tier's restore speed is the limit,** not the GPU: about 0.65-0.8 GB/s from LMCache's CPU backend, far below the GPU's PCIe link. Larger LMCache chunks speed up offload (1.3 → 2.0 GB/s) but not restore, and they cost short contexts (a chunk is stored only when full).
- Two 32k-token restore readings (0.05 s and 0.10 s for 1.8 GB) were physically impossible and are excluded; the chunk-2048 run measured 32k normally (2.2 s).

### 2. Simulated chaos benchmark (replicas replay the measured rates)

**These are not real-GPU results.** The replicas are simulators that replay the measurements above: prefill time from the measured curve, cache restore speed, and the GPU's KV capacity. Kubernetes, the operator, the router, the sandboxes and their volumes are real (a kind cluster with 4 worker nodes). Each run per mode: 3 minutes of agent load, a node drain, a spot interruption (10 s notice), a consolidation and a scale-down, then 4 sessions hibernate and wake. **3 seeds**; counts are summed and latencies averaged.

#### Profile `qwen7b-l4`: moving a cache is cheaper than recomputing it

7B on L4 rates, about 50k tokens of KV memory per replica, 10 sessions with 4k-12k tokens of starting context. Raw log: `results/bench-qwen7b-l4-20261112-215135.log`.

| | plain Kubernetes | sandbox persistence only | cache routing only | **Anton** |
|---|---|---|---|---|
| Disrupted sessions that lost their KV cache | 25 of 36 | 27 of 31 | 30 of 30 | **0 of 28** |
| Spot interruption: KV caches that survived the notice | 1 of 9 | 1 of 8 | 0 of 8 | **8 of 8** |
| Disrupted sandboxes that lost their workspace | 27 of 27 | 0 of 27 | 27 of 27 | **0 of 27** |
| Resume time to first token after a disruption, p50 | 3.3 s | 3.7 s | 4.3 s | **1.5 s** |
| Prefill GPU-seconds per request (mean of seeds ± sd) | 2.26 ± 0.24 | 2.23 ± 0.22 | 0.58 ± 0.15 | **0.48 ± 0.13** |
| Time to first token, p50 / p95 | 2.6 / 4.9 s | 2.6 / 5.0 s | 0.13 / 4.7 s | 0.13 / 4.1 s |
| Hibernate → wake: KV cache kept | 2 of 12 | 5 of 12 | 5 of 12 | **11 of 12** |
| Hibernate → wake: workspace kept | 0 of 12 | 12 of 12 | 0 of 12 | **12 of 12** |

- **Each existing approach keeps one kind of state.** Sandbox persistence keeps every workspace but loses KV caches like plain Kubernetes. Cache-aware routing cuts prefill work by 74%, but it pins each session to one replica, so every disrupted session lost its cache (30 of 30). Anton kept all 28 disrupted caches and all 27 workspaces, and it resumed disrupted sessions almost 3× faster than routing alone.
- **Against routing alone, the steady-state saving is modest.** Anton used fewer prefill GPU-seconds per request than routing-only in every seed (0.61 vs 0.70, 0.35 vs 0.41, 0.50 vs 0.62), about 17% fewer on average, which is about the size of the seed-to-seed spread.
- **The L4's small KV memory still shows.** p95 latency is high in every mode because replicas evict caches to make room, and one of Anton's 12 wakes found its cache evicted.

#### Profile `qwen7b-a100`: recomputing is cheaper, so Anton should not move caches

A100 rates, about 300k tokens of KV memory per replica, 24 sessions. Raw log: `results/bench-qwen7b-a100-20261112-210723.log`.

| | plain Kubernetes | sandbox persistence only | cache routing only | **Anton** |
|---|---|---|---|---|
| Disrupted sessions that lost their KV cache | 53 of 88 | 45 of 70 | 68 of 68 | 73 of 73 |
| Disrupted sandboxes that lost their workspace | 66 of 66 | 0 of 66 | 66 of 66 | **0 of 66** |
| Prefill GPU-seconds per request (mean of seeds ± sd) | 0.73 ± 0.01 | 0.72 ± 0.02 | 0.08 ± 0.00 | 0.09 ± 0.00 |
| Resume time to first token after a disruption, p50 | 1.3 s | 1.3 s | 1.9 s | 2.1 s |
| Hibernate → wake: KV cache kept | 8 of 12 | 7 of 12 | 12 of 12 | 12 of 12 |
| Hibernate → wake: workspace kept | 0 of 12 | 12 of 12 | 0 of 12 | **12 of 12** |

- **This is the expected result.** On an A100, recomputing 16k tokens takes 1.2 s and moving the cache takes 2.1 s, so Anton's rule lets disrupted caches go and recomputes them, and it behaves like routing-only (within 0.01 GPU-seconds per request, about 5% more). It still keeps every workspace.
- Plain Kubernetes and sandbox-only lose a smaller share of caches in disruptions here (53 of 88 and 45 of 70), but they recompute about 9× more tokens per request than routing in steady state, so that share does not make them cheaper.

An earlier A100 run (`results/superseded-bench-qwen7b-a100-20261112-193204.log`) and the first L4 run (`results/bench-qwen7b-l4-20261112-094309.log`) used a router that moved sessions off a leaving replica before it was gone and waited for rescues the guard had not started. Both bugs are fixed and tested (`cmd/router/pick_test.go`); those logs are kept only for the record. Earlier runs in `results/bench-20261112-*.log` used a calibration from plain Hugging Face transformers, about 4× slower than vLLM, and overstate Anton's benefit.

### 3. Real GPUs on EKS: agents through a drain, a hard kill and a real spot interruption

**These are real results from a small run.** EKS 1.37 in us-east-2, Karpenter, two `g4dn.xlarge` spot nodes (one T4 16 GB each), vLLM 0.31.0 + LMCache 0.5.6 serving Qwen2.5-7B-Instruct-AWQ, a shared LMCache server on a CPU node, EBS volumes for agent workspaces. 2026-11-13, one run per mode, 4 to 6 sessions each, no repeats. The model is the 4-bit AWQ build because the fp16 weights (15.2 GB) do not fit a T4, and L4 spot had no capacity. Scripted in `deploy/eks/run.sh` (profile `deploy/bench/profiles/eks-t4`); raw logs in `results/eks-20261113-*` and `results/bench-eks-t4-20261113-*`.

**A coding agent works end to end.** OpenCode 1.19.5 in a sandbox, through the session router, on the real model: the first turn wrote `hello.py` with a real tool call (7,611 prompt tokens, none cached, 44 to 46 s); the second turn edited it with 7,696 of 7,722 prompt tokens served from the KV cache (4.2 s). Four separate clusters gave the same token counts.

**Sessions survive losing their node.** Builtin agent, 6 sessions, each with about 11,600 prompt tokens of which all but the first 750 are its own (a per-session "repository"). A drain of one GPU node, then a hard kill of the other; separately, a real spot interruption sent by AWS FIS. Each figure is the first turn after the disruption, for sessions whose sandbox was on the lost node.

| Disruption (Anton mode) | Sessions hit | Conversation intact | Next turn worked | Prompt tokens recomputed on that turn |
|---|---|---|---|---|
| Drain | 2 of 6 | 2 of 2 | 2 of 2 | 368 and 627 |
| Hard kill | 3 of 6 | 3 of 3 | 3 of 3 | 622, 629 and 789 |
| **Real AWS spot interruption** | 2 of 6 | 2 of 2 | 2 of 2 | 375 and 631 |

On the spot interruption AWS sent the notice at 19:32:46; 16 seconds later Anton's guard had offloaded the dying replica's 3 session caches (1.89 GB, inside a 17.9 s budget). Sandboxes were back on another node in about 30 s (median), with their EBS workspaces. OpenCode sessions went through the same drain and hard kill in an earlier run the same day: 4 of 4 conversations intact and next turns working.

**What the shared KV tier is worth.** The same drain and hard kill, comparing the first turn after a session lost its replica:

| | Prompt tokens recomputed (of about 11,300) | Whole reply |
|---|---|---|
| Shared LMCache tier on: plain Kubernetes | 414 to 804 | 3.6 to 16.1 s |
| Tier on: sandbox persistence only | 363 to 797 | 5.5 to 12.3 s |
| Tier on: cache routing only | 370 to 796 | 6.5 to 17.3 s |
| Tier on: **Anton** | 368 to 789 | 11.1 to 17.6 s |
| **Tier off**, sessions pinned to a replica (4 sessions that lost theirs) | **10,374 to 11,158** | **51.5 to 56.1 s** |

- **The tier is the difference: about 10,000 fewer prompt tokens and about 40 seconds per resumed turn** on a T4, for a session with an 11,000-token context of its own. A fifth session in the tier-off run kept its replica and recomputed 364 tokens.
- **The four modes did not separate on real GPUs, and that is the honest reading.** With the tier on, plain Kubernetes recovered as well as Anton. LMCache writes KV chunks through to the shared server as they are computed, so a surviving replica fetches them whether or not anyone rescued them, and every mode here had EBS workspaces. The mode gap in section 2 comes from simulated replicas that save a cache only when asked. What this run shows for Anton is that its pieces work on real infrastructure (the session router in front of vLLM, the guard's rescue against a real interruption, sandboxes and workspaces moving between nodes), not that they beat a plain deployment that already has a shared tier.
- **"Whole reply" is prefill plus 256 generated tokens**, not time to first token; do not compare it with section 2. The spread within a row is mostly queueing behind other sessions on the one surviving GPU.
- **Small numbers.** Two to five sessions per row, one run each; these are observations, not rates.
- An earlier run that night gave every session the same 6,000-token system prompt, which made every cache look warm on any replica; those logs (`bench-eks-t4-20261113-09*`, `-16*`) are kept but not used above.
- The cost model's rates in this profile are borrowed from the L4 fp16 calibration; they were not measured on a T4.

## How it works

```
 web UI ── FastAPI ──creates──▶ AgentSession (CRD) ◀──reconciles── operator ──▶ sandbox pod
              │                        ▲                              │
              │                 sync worker ──▶ Postgres              └─ guard ──offload──▶ replica ──▶ cache tier
              └──chat──▶ router ──affinity / restore──▶ inference replicas ◀──────restore─────────────┘
```

| Component | Code | Role |
|---|---|---|
| `AgentSession` CRD | `api/v1alpha1`, `deploy/helm/anton/templates/crd.yaml` | One resource owning sandbox state and KV-cache identity. Phases: Pending, Active, Hibernating, Hibernated, Waking, Migrating, Rescuing, Failed. |
| Operator | `internal/controller/session.go`, `workspace.go` | Creates the sandbox, detects idleness, hibernates (VolumeSnapshot or tarball, KV offload, pod and volume deleted) and wakes (volume restored from the snapshot). Leader-elected. |
| Disruption guard | `internal/controller/guard.go` | Writes each replica's cache value as `pod-deletion-cost`; on a spot notice, cordon or pod deletion, plans and runs KV offloads before the deadline. |
| Planner | `internal/planner` | The decision rules (below). Pure Go, unit-tested. |
| Router | `cmd/router` | Session affinity, cost-based affinity breaking, wake-on-request, KV restore before first token. |
| Simulator | `cmd/sim` | Mock inference replica (vLLM-shaped metrics), cache tier, and the sandbox agent (see Limitations). |
| API + sync worker | `api/app` | Google OAuth or dev login, teams and memberships, hashed API keys, quotas, templates, audit log, rate limits (Redis), sessions, chat, workspace terminal and files, ops, benchmarks. Alembic migrations; CloudNativePG in the cluster. |
| Web UI | `web` | Sessions, agent workspace (chat, live terminal, file tree, timeline, "moved and came back in" indicator), team admin, ops dashboard, chaos lab with charts. |
| Benchmark | `cmd/bench` | Seeded workload, four disruptions, four modes, several seeds. |
| Packaging | `deploy/helm/anton`, `deploy/install.sh`, `.github/workflows/ci.yml` | Helm chart with least-privilege RBAC, PDBs, NetworkPolicies, ServiceMonitors and a Grafana dashboard; one-command kind install; CI with envtest. |

Design rationale, the state machine and failure modes: [docs/design.md](docs/design.md).

### The three decision rules

1. **Offload or recompute.** Moving a KV cache is only worth it when `offload time + restore time < prefill time`. All three terms come from measured rates.
2. **Cache value.** A cache entry is worth `P(reuse) × prefill seconds avoided`, where `P(reuse)` decays with time since last use and rises with how often the prefix was hit. A replica's score is the sum over its sessions; the lowest-scoring replica is the cheapest to remove.
3. **Rescue under a deadline.** Offloads from one replica share its outbound bandwidth, so a deadline is a byte budget: a knapsack with value = net prefill saved and weight = bytes. Anton serves higher priority tiers first, then greedy by value per byte. An exact knapsack (`PlanDP`) is kept as the yardstick; in 200 random cases greedy reached at least 69% of the optimum.

Failure rule throughout: **fail to recompute**. A failed offload or restore never blocks a drain or a request; the session just pays the prefill.

### Source of truth

Kubernetes owns live state; Postgres owns history, identity and quotas. The API writes the `AgentSession` resource first, the operator acts, and the sync worker records what happened, skipping any event whose `resourceVersion` it has already stored. If the two disagree, the cluster wins: the worker's relist repairs Postgres.

## Run it

**Locally on kind** (needs docker, kind, kubectl and helm; mock-GPU mode):

```bash
deploy/install.sh                 # kind cluster + CloudNativePG + Anton
MONITORING=1 deploy/install.sh    # ...plus Prometheus and Grafana (dashboard "Anton Platform")
SNAPSHOTS=1 deploy/install.sh     # ...plus CSI hostpath + VolumeSnapshots; sandboxes use PVC workspaces
kubectl -n anton port-forward svc/anton-api 8000:8000   # then open http://localhost:8000
```

**On one EC2 instance** (needs the AWS CLI; the instance runs the same installer):

```bash
deploy/aws/up.sh          # create the instance (m7i-flex.large)
deploy/aws/run.sh         # copy the repo, install everything
deploy/aws/run.sh bench   # ...and run the chaos benchmark; results land in ./results
deploy/aws/down.sh        # terminate the instance and clean up
```

Google sign-in: create an OAuth client (type "Web application", redirect URI `<public URL>/api/auth/google/callback`), store it as a Secret with keys `client_id` and `client_secret`, and install with `--set api.googleOAuthSecret=<secret> --set api.publicURL=<public URL> --set api.devLogin=false`.

Tests: `go test ./...` (operator tests need envtest: see the header of `internal/controller/session_test.go`), `cd api && pip install -r requirements-dev.txt && pytest`, `cd web && npx tsc --noEmit`. CI runs all of them plus Helm lint and image builds.

## The benchmark

Each mode runs the same seeded workload: 24 agent sessions, 8k-40k tokens of starting context growing every turn, 2-6 s of think time, on 4 inference replicas across 4 worker nodes. Each run gets the same three disruptions:

| Disruption | What happens |
|---|---|
| Node drain | Cordon a node, delete its pods with normal grace. |
| Spot interruption | A 10-second notice, then the node's pods are force-killed. |
| Scale-down | The replica Deployment loses one replica; Kubernetes picks the victim. |

Afterwards four sessions are hibernated and woken by a request.

| Mode | Routing | Workspace survives | KV rescue | Stands in for |
|---|---|---|---|---|
| `plain` | random | no | no | a plain Kubernetes Deployment + Service |
| `sandbox` | random | yes | no | Agent Sandbox alone |
| `routing` | cache affinity | no | no | cache-aware routing alone (llm-d style) |
| `anton` | cache affinity | yes | yes | the full system |

A session counts as having **lost KV state** when its first request after losing its replica finds less than half of its previous context cached.

## Limitations

Read these before quoting any number.

- **Inference is simulated in the cluster benchmark of section 2.** There, replicas are mock servers that replay measured vLLM + LMCache timings (prefill curve, offload and restore rates, KV capacity) and track a per-session KV cache; no model runs inside the cluster. The measurements are real (section 1), but queueing, batching and memory effects inside a real vLLM server are only approximated.
- **The cache tier in the measurements is LMCache's CPU backend on the GPU's own host.** In the cluster the tier is across the network, which would make moving caches slower than measured.
- **Section 2 runs on one machine.** The cluster is kind on a single 2-vCPU EC2 instance. Drains go through the real Eviction API and PDBs; a "spot interruption" is an annotation followed by a force-delete; "consolidation" imitates Karpenter rather than running it. Section 3 is the run on EKS with Karpenter, real GPUs and a real interruption; it is one small run, not a benchmark.
- **The cache tier is in memory** on the node that is never disrupted.
- **PVC workspaces on kind are node-pinned.** The CSI hostpath driver keeps volumes on one node, so with `SNAPSHOTS=1` sandboxes cannot follow a drain to another node; that needs network storage (EBS, PD). The benchmark therefore uses tarball workspaces on kind.
- **The router is a standalone Go proxy**, not a Gateway API Inference Extension endpoint picker.
- **Per-team namespaces are opt-in** (`--set tenancy.enabled=true`, see [docs/multitenancy.md](docs/multitenancy.md)). By default every session lives in one namespace and tenancy is enforced in the API (every lookup filters by team). With the switch on, each team gets its own namespace with RBAC, a default-deny NetworkPolicy and a quota, and the shared replicas' cache is salted per team; this is verified on kind with `deploy/tenancy-check.sh`, not on real GPUs, and the benchmark still runs in the platform namespace. Sandboxes are isolated by a default-deny NetworkPolicy in both modes.
- **Two sandbox agents.** A minimal builtin agent loop, and OpenCode (`sandbox.agent=opencode`). The simulated benchmark's load does not go through either; the recovery scenario and section 3 do.
- A deleted replica is kept alive for 20 s by a preStop hook so the guard can rescue it; that delays every replica shutdown by up to 20 s.
