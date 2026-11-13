"""Thin wrapper around the Kubernetes API. The product API never writes live
state to Postgres first: it writes the AgentSession resource here, the
operator acts, and the sync worker records what happened."""
import json
import os
import re
import time

from kubernetes import client, config
from kubernetes.stream import stream

NS = "anton"
# Multi-tenancy (docs/multitenancy.md): with TENANCY=1 a team's sessions and
# sandboxes live in the namespace anton-team-<team>. The API creates that
# namespace; the operator's tenant controller then gives the API, router and
# operator access to it and isolates its sandboxes.
TENANCY = os.environ.get("TENANCY", "") == "1"
TENANT_PREFIX = "anton-team-"
GROUP, VERSION, PLURAL = "anton.dev", "v1alpha1", "agentsessions"
SPOT_ANNOTATION = "anton.dev/spot-deadline"
DELETION_COST = "controller.kubernetes.io/pod-deletion-cost"

try:
    config.load_incluster_config()
except config.ConfigException:
    try:
        config.load_kube_config()
    except config.ConfigException:
        pass  # no cluster available (local import / tests)

core = client.CoreV1Api()
batch = client.BatchV1Api()
custom = client.CustomObjectsApi()


_session_ns: dict[str, str] = {}  # session name -> namespace (names are cluster-unique)


def team_namespace(team: str) -> str:
    """The namespace for a team's sessions: a valid DNS label built from the
    team name."""
    slug = re.sub(r"[^a-z0-9-]+", "-", team.lower()).strip("-") or "default"
    return (TENANT_PREFIX + slug)[:63].rstrip("-")


def ensure_team_namespace(team: str) -> str:
    """Creates the team's namespace if it is missing. Only names and labels
    of this shape pass the cluster's admission policy for the API."""
    ns = team_namespace(team)
    try:
        core.create_namespace(client.V1Namespace(metadata=client.V1ObjectMeta(
            name=ns, labels={"anton.dev/tenant": "true", "anton.dev/team": team})))
    except client.ApiException as e:
        if e.status != 409:  # already there
            raise
    return ns


def session_namespace(name: str) -> str:
    """Where a session lives. Looked up once across namespaces, then cached."""
    if not TENANCY:
        return NS
    if name in _session_ns:
        return _session_ns[name]
    try:
        items = custom.list_cluster_custom_object(
            GROUP, VERSION, PLURAL, field_selector=f"metadata.name={name}").get("items", [])
    except client.ApiException:
        items = []
    if items:
        _session_ns[name] = items[0]["metadata"]["namespace"]
        return _session_ns[name]
    return NS


def create_session(name: str, team: str, model: str, priority: int, idle_timeout: int) -> dict:
    ns = ensure_team_namespace(team) if TENANCY else NS
    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "AgentSession",
        "metadata": {"name": name, "namespace": ns, "labels": {"anton.dev/team": team}},
        "spec": {"team": team, "model": model, "priority": priority,
                 "idleTimeoutSeconds": idle_timeout, "desiredState": "Active"},
    }
    # A namespace created a moment ago has no RoleBinding for the API yet (the
    # operator's tenant controller adds it within a second or two): retry the
    # "forbidden" that this causes, and nothing else.
    for attempt in range(40):
        try:
            out = custom.create_namespaced_custom_object(GROUP, VERSION, ns, PLURAL, body)
            _session_ns[name] = ns
            return out
        except client.ApiException as e:
            if not (TENANCY and e.status == 403 and attempt < 39):
                raise
            time.sleep(0.5)


def set_desired(name: str, state: str) -> dict:
    return custom.patch_namespaced_custom_object(
        GROUP, VERSION, session_namespace(name), PLURAL, name, {"spec": {"desiredState": state}})


def delete_session(name: str) -> None:
    custom.delete_namespaced_custom_object(GROUP, VERSION, session_namespace(name), PLURAL, name)
    _session_ns.pop(name, None)


def list_sessions() -> dict:
    if TENANCY:  # every team namespace, and the platform's own (benchmark sessions)
        return custom.list_cluster_custom_object(GROUP, VERSION, PLURAL)
    return custom.list_namespaced_custom_object(GROUP, VERSION, NS, PLURAL)


def watch_sessions_args() -> tuple:
    """The list call and arguments the sync worker watches with."""
    if TENANCY:
        return custom.list_cluster_custom_object, (GROUP, VERSION, PLURAL)
    return custom.list_namespaced_custom_object, (GROUP, VERSION, NS, PLURAL)


def get_mode() -> str:
    return (core.read_namespaced_config_map("anton-mode", NS).data or {}).get("mode", "anton")


def set_mode(mode: str) -> None:
    core.patch_namespaced_config_map("anton-mode", NS, {"data": {"mode": mode}})


def sandbox_ready(session: str) -> str | None:
    """The sandbox's IP once it is Ready (its agent is serving), else None."""
    try:
        p = core.read_namespaced_pod(f"{session}-sandbox", session_namespace(session))
    except client.ApiException:
        return None
    ready = any(c.type == "Ready" and c.status == "True" for c in (p.status.conditions or []))
    return p.status.pod_ip if ready and p.metadata.deletion_timestamp is None else None


def sandbox_ip(session: str) -> str | None:
    try:
        return core.read_namespaced_pod(f"{session}-sandbox", session_namespace(session)).status.pod_ip
    except client.ApiException:
        return None


def exec_shell(session: str):
    """Opens an interactive shell in the session's sandbox (a websocket-backed
    client with stdin/stdout channels and a resize channel)."""
    return stream(core.connect_get_namespaced_pod_exec, f"{session}-sandbox", session_namespace(session), container="agent",
                  command=["/bin/sh", "-c", "cd /workspace 2>/dev/null; exec bash -l 2>/dev/null || exec sh -l"],
                  stdin=True, stdout=True, stderr=True, tty=True, _preload_content=False)


def ops_snapshot() -> dict:
    nodes = []
    for n in core.list_node().items:
        ann = n.metadata.annotations or {}
        nodes.append({
            "name": n.metadata.name,
            "control_plane": "node-role.kubernetes.io/control-plane" in (n.metadata.labels or {}),
            "cordoned": bool(n.spec.unschedulable),
            "spot_deadline": ann.get(SPOT_ANNOTATION),
        })
    replicas = []
    for p in core.list_namespaced_pod(NS, label_selector="app=anton-replica").items:
        ready = any(c.type == "Ready" and c.status == "True" for c in (p.status.conditions or []))
        replicas.append({
            "name": p.metadata.name,
            "node": p.spec.node_name,
            "ready": ready and p.metadata.deletion_timestamp is None,
            "terminating": p.metadata.deletion_timestamp is not None,
            "deletion_cost": (p.metadata.annotations or {}).get(DELETION_COST),
        })
    return {"nodes": nodes, "replicas": replicas, "mode": get_mode()}


def launch_bench(job: str, duration: str, sessions: int, seeds: str = "1", spot_deadline: str = "10s") -> None:
    cp = {"node-role.kubernetes.io/control-plane": ""}
    spec = client.V1PodSpec(
        service_account_name="anton-bench", restart_policy="Never", node_selector=cp,
        tolerations=[client.V1Toleration(key="node-role.kubernetes.io/control-plane", operator="Exists", effect="NoSchedule")],
        containers=[client.V1Container(
            name="bench", image="anton:dev", image_pull_policy="IfNotPresent",
            command=["/bin/bench", "--modes=plain,sandbox,routing,anton", f"--sessions={sessions}",
                     f"--duration={duration}", "--replicas=4", f"--spot-deadline={spot_deadline}",
                     "--hibernate=4", f"--seeds={seeds}"])])
    batch.create_namespaced_job(NS, client.V1Job(
        metadata=client.V1ObjectMeta(name=job, namespace=NS),
        spec=client.V1JobSpec(backoff_limit=0, template=client.V1PodTemplateSpec(
            metadata=client.V1ObjectMeta(labels={"app": "anton-bench", "job": job}), spec=spec))))


def bench_status(job: str) -> tuple[str, list]:
    """Returns (running|succeeded|failed, parsed ANTON_RESULT lines)."""
    try:
        st = batch.read_namespaced_job(job, NS).status
    except client.ApiException:
        return "failed", []
    status = "succeeded" if st.succeeded else "failed" if st.failed else "running"
    results = []
    pods = core.list_namespaced_pod(NS, label_selector=f"job={job}").items
    if pods:
        try:
            for line in core.read_namespaced_pod_log(pods[0].metadata.name, NS).splitlines():
                if line.startswith("ANTON_RESULT "):
                    results.append(json.loads(line[len("ANTON_RESULT "):]))
        except client.ApiException:
            pass
    return status, results
