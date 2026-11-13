# Multi-tenancy design: one namespace per team

Status: **design, not yet implemented.** Today every session lives in the
`anton` namespace and tenancy is enforced in the API (every lookup filters by
team). This document is the plan for real Kubernetes isolation and the
reasoning behind it, written before the code because the cache and RBAC
choices are easy to get subtly wrong.

## Goals

1. A team's sandboxes, workspaces and snapshots live in that team's namespace,
   isolated by Kubernetes (RBAC, NetworkPolicy, ResourceQuota), not only by
   API code.
2. Platform components keep **least privilege**: no component gets cluster-wide
   write access to pods, and none can grant itself more than it was given.
3. Shared inference replicas must not leak one team's prompts or timing to
   another.
4. Adding a team needs no operator redeploy.

## Layout

| Namespace | Holds |
|---|---|
| `anton` (platform) | operator, router, inference replicas, cache tier, API, sync worker, Postgres, Redis |
| `anton-team-<team>` (one per team) | the team's `AgentSession` objects, sandbox pods, workspace PVCs and VolumeSnapshots, its NetworkPolicies, ResourceQuota and LimitRange |

Inference replicas stay shared in `anton`: GPUs are the expensive pool and
sharing them is the point of a platform. Session names stay cluster-unique
(they already carry the team prefix), so the router can look a session up by
name alone.

## Who provisions a team namespace

1. The **API** creates the namespace when a team is created, labelled
   `anton.dev/tenant: "true"` and `anton.dev/team: <team>`.
2. The **operator's tenant controller** watches namespaces with that label and
   provisions each one:
   - RoleBindings that grant the platform service accounts the
     `anton-tenant-*` ClusterRoles **in that namespace only**;
   - a default-deny NetworkPolicy for sandboxes (ingress from the operator and
     API in `anton`; egress to the router, the cache tier and DNS; nothing to
     other team namespaces);
   - a ResourceQuota (pods, PVCs, storage, snapshot count) from the team's
     quota, and a LimitRange with sandbox defaults.

## RBAC

ClusterRoles that are **never bound cluster-wide**, only through RoleBindings
in team namespaces:

| ClusterRole | Bound to | Grants, inside one team namespace |
|---|---|---|
| `anton-tenant-operator` | operator | AgentSessions (+status), pods, PVCs, VolumeSnapshots, events |
| `anton-tenant-api` | API, sync worker | AgentSessions create/patch/delete/watch, pods get, pods/exec (terminal) |
| `anton-tenant-router` | router | AgentSessions get/patch (+status) |

Cluster-scoped permissions, each as narrow as RBAC allows:

| Component | Cluster permission | Why it cannot be narrower, and the guard |
|---|---|---|
| API | `namespaces: create` | Needed to create team namespaces. A **ValidatingAdmissionPolicy** (CEL) only admits namespaces from the API service account named `anton-team-*` with the tenant labels. |
| operator (tenant controller) | `namespaces: get/list/watch`; `rolebindings: create/update`; `bind` on the three `anton-tenant-*` ClusterRoles only (`resourceNames`) | Kubernetes' escalation check means it can only bind those three roles, never anything broader. RBAC cannot limit *which namespaces* it binds them in, so a second **ValidatingAdmissionPolicy** rejects any RoleBinding to an `anton-tenant-*` role in a namespace without `anton.dev/tenant: "true"`. Without this, a compromised operator could grant itself pod access in `kube-system`. |
| operator, router | `nodes: get/list/watch` | Unchanged: spot notices and cordons are node-level. |

Everything else the operator does (pods, PVCs, snapshots) is namespaced, through the RoleBindings above.

## The operator's cache

controller-runtime caches are built once, from a fixed namespace list, and an
informer for a namespace the service account cannot list fails forever. Three
options:

| Option | Permissions needed | Problem |
|---|---|---|
| A. Cluster-wide cache | `pods: list/watch` cluster-wide | Reads every pod in the cluster (including other systems' pod specs and env). Rejected. |
| B. Cluster-wide cache, label-filtered | same as A | The label filter limits memory, not permission: RBAC cannot grant "list pods with label X". Rejected for the same reason. |
| **C. Namespaced cache over tenant namespaces, rebuilt when the set changes** | only the namespaced RoleBindings | Chosen. |

**Option C in detail.** At startup the operator lists namespaces labelled
`anton.dev/tenant: "true"` (it may list namespaces) and builds its cache with
`DefaultNamespaces` = `anton` plus those namespaces. The tenant controller
watches namespaces; when a new tenant namespace has finished provisioning (its
RoleBindings exist), the operator triggers a graceful restart of its own
manager. With two leader-elected replicas the standby takes over within the
Lease timeout, so a new team costs one leader hand-off of about 15 s; team
creation is rare, and sessions keep running during it (the data plane does
not depend on the operator). The same applies to the router's cache.

Rejected alternative: per-namespace informers added at runtime with a custom
cache. It avoids the restart but replaces a well-tested controller-runtime
path with custom code for an event that happens a few times a day.

## Shared replicas: preventing cross-tenant cache leaks

vLLM's prefix cache is keyed by token content, so two teams sending the same
prefix share KV blocks, and a team can detect another team's prompt by timing
(a cache hit returns its first token faster). vLLM supports a per-request
`cache_salt` that partitions the prefix cache. The router will set
`cache_salt = <team>` on every chat request it forwards (it already reads the
body to estimate length; the team comes from the session's namespace label),
so caches are only ever shared within a team. LMCache entries carry the same
salt because they are keyed on vLLM's block hashes.

## Other changes this implies

- `AgentSession` names: unchanged (team-prefixed, cluster-unique).
- Router: looks sessions up by name across its cached namespaces (a field
  index on `metadata.name`), not with a fixed namespace.
- API: `kube.py` takes the team's namespace for every call; the sync worker
  lists sessions across tenant namespaces with a label selector.
- Benchmark: runs in a dedicated `anton-team-bench` namespace.
- Migration: existing sessions in `anton` keep working until hibernated; on
  wake the API recreates them in the team namespace (sessions are cheap to
  recreate; workspaces move with their snapshot).

## Test plan

- envtest: the tenant controller creates exactly the RoleBindings,
  NetworkPolicies and quota; a cache built from two tenant namespaces sees
  sessions in both and none elsewhere.
- envtest with the ValidatingAdmissionPolicies installed: a RoleBinding to
  `anton-tenant-operator` in an unlabelled namespace is rejected; the API
  service account cannot create `kube-*` namespaces.
- On the cluster, impersonating each service account (`kubectl auth can-i
  --as=system:serviceaccount:anton:anton-operator`): no cluster-wide pod
  access, full access in a team namespace, none in another platform's.
- A cross-tenant probe: team A's sandbox cannot reach team B's sandbox, and
  the same prompt from two teams does not produce a cache hit.
