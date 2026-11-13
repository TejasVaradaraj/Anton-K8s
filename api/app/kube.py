"""Thin wrapper around the Kubernetes API. The product API never writes live
state to Postgres first: it writes the AgentSession resource here, the
operator acts, and the sync worker records what happened."""
import json

from kubernetes import client, config
from kubernetes.stream import stream

NS = "anton"
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


def create_session(name: str, team: str, model: str, priority: int, idle_timeout: int) -> dict:
    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "AgentSession",
        "metadata": {"name": name, "namespace": NS, "labels": {"anton.dev/team": team}},
        "spec": {"team": team, "model": model, "priority": priority,
                 "idleTimeoutSeconds": idle_timeout, "desiredState": "Active"},
    }
    return custom.create_namespaced_custom_object(GROUP, VERSION, NS, PLURAL, body)


def set_desired(name: str, state: str) -> dict:
    return custom.patch_namespaced_custom_object(
        GROUP, VERSION, NS, PLURAL, name, {"spec": {"desiredState": state}})


def delete_session(name: str) -> None:
    custom.delete_namespaced_custom_object(GROUP, VERSION, NS, PLURAL, name)


def list_sessions() -> dict:
    return custom.list_namespaced_custom_object(GROUP, VERSION, NS, PLURAL)


def get_mode() -> str:
    return (core.read_namespaced_config_map("anton-mode", NS).data or {}).get("mode", "anton")


def set_mode(mode: str) -> None:
    core.patch_namespaced_config_map("anton-mode", NS, {"data": {"mode": mode}})


def sandbox_ready(session: str) -> str | None:
    """The sandbox's IP once it is Ready (its agent is serving), else None."""
    try:
        p = core.read_namespaced_pod(f"{session}-sandbox", NS)
    except client.ApiException:
        return None
    ready = any(c.type == "Ready" and c.status == "True" for c in (p.status.conditions or []))
    return p.status.pod_ip if ready and p.metadata.deletion_timestamp is None else None


def sandbox_ip(session: str) -> str | None:
    try:
        return core.read_namespaced_pod(f"{session}-sandbox", NS).status.pod_ip
    except client.ApiException:
        return None


def exec_shell(session: str):
    """Opens an interactive shell in the session's sandbox (a websocket-backed
    client with stdin/stdout channels and a resize channel)."""
    return stream(core.connect_get_namespaced_pod_exec, f"{session}-sandbox", NS, container="agent",
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
