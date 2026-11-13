"""Pre-flight for deploy/eks/run.sh, before anything is paid for.

Renders the chart exactly as run.sh installs it and checks that every
workload can be scheduled on the nodes this cluster will have, and that
every volume claim gets a StorageClass. Exits non-zero with the reasons.

    python3 deploy/eks/preflight.py <helm template args...>
"""
import subprocess
import sys

import yaml

# The node kinds the EKS cluster has (main.tf system node group, gpu-nodepool.yaml).
NODES = {
    "system": {"labels": {"anton.dev/role": "system"}, "taints": []},
    "gpu": {"labels": {"anton.dev/gpu": "true", "karpenter.sh/nodepool": "gpu"},
            "taints": [{"key": "nvidia.com/gpu", "effect": "NoSchedule"}]},
}
DEFAULT_STORAGE_CLASS = "anton-gp3"   # storage.yaml marks it the default


def tolerates(tols, taint):
    for t in tols or []:
        if t.get("operator") == "Exists" and (not t.get("key") or t["key"] == taint["key"]) \
                and (not t.get("effect") or t["effect"] == taint["effect"]):
            return True
        if t.get("key") == taint["key"] and t.get("value") == taint.get("value") \
                and (not t.get("effect") or t["effect"] == taint["effect"]):
            return True
    return False


def fits(selector, tols, node):
    if any(node["labels"].get(k) != str(v) for k, v in (selector or {}).items()):
        return False
    return all(tolerates(tols, t) for t in node["taints"])


def pod_specs(doc):
    kind = doc.get("kind")
    if kind in ("Deployment", "StatefulSet", "DaemonSet", "Job"):
        yield doc["spec"]["template"]["spec"]
    elif kind == "CronJob":
        yield doc["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    elif kind == "Pod":
        yield doc["spec"]
    elif kind == "Cluster" and doc.get("apiVersion", "").startswith("postgresql.cnpg.io"):
        aff = doc["spec"].get("affinity", {})
        yield {"nodeSelector": aff.get("nodeSelector"), "tolerations": aff.get("tolerations")}


def main():
    out = subprocess.run(["helm", "template", *sys.argv[1:]], check=True, capture_output=True, text=True).stdout
    problems = []
    for doc in yaml.safe_load_all(out):
        if not doc:
            continue
        name = f'{doc["kind"]}/{doc["metadata"]["name"]}'
        for spec in pod_specs(doc):
            where = [n for n, node in NODES.items() if fits(spec.get("nodeSelector"), spec.get("tolerations"), node)]
            print(f"{name:45s} nodeSelector={spec.get('nodeSelector')} -> {where or 'NOWHERE'}")
            if not where:
                problems.append(f"{name} fits no node: nodeSelector={spec.get('nodeSelector')}")
        for vct in (doc.get("spec", {}) or {}).get("volumeClaimTemplates", []) or []:
            if not vct["spec"].get("storageClassName") and not DEFAULT_STORAGE_CLASS:
                problems.append(f"{name} volume claim has no StorageClass")
        if doc.get("kind") == "PersistentVolumeClaim" and not doc["spec"].get("storageClassName") and not DEFAULT_STORAGE_CLASS:
            problems.append(f"{name} has no StorageClass")
    if problems:
        print("\nPREFLIGHT FAILED:\n  " + "\n  ".join(problems))
        sys.exit(1)
    print("\nPREFLIGHT OK")


main()
