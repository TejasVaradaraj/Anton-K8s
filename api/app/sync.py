"""The sync worker: watches AgentSession resources and mirrors them into
Postgres. Writes are idempotent and keyed on resourceVersion, so replaying
the same watch event (or re-listing after a restart) changes nothing.
If Postgres and the cluster disagree, the cluster wins."""
import logging
import threading
import time

from kubernetes import watch

from . import kube
from .db import LifecycleEvent, SessionLocal, SessionRow, now

log = logging.getLogger("sync")


def apply(obj: dict, deleted: bool = False) -> None:
    meta, spec, status = obj["metadata"], obj.get("spec", {}), obj.get("status", {})
    rv = meta.get("resourceVersion", "")
    team = spec.get("team", "")
    with SessionLocal() as db:
        row = db.get(SessionRow, meta["name"])
        if row is None or not row.resource_version:
            # New to the worker. The API may have inserted the row first (it
            # records the owner), so "not yet synced" also counts as new.
            if row is None:
                row = SessionRow(name=meta["name"])
                db.add(row)
            row.phase, row.replica, row.cache_location = "", "", ""
            db.add(LifecycleEvent(session=row.name, team=team, kind="Created", detail=f"team {team}"))
        elif row.resource_version == rv and row.deleted == deleted:
            return  # already recorded: this is what makes the worker idempotent

        def event(kind: str, detail: str = "") -> None:
            db.add(LifecycleEvent(session=row.name, team=team, kind=kind, detail=detail))

        phase = status.get("phase", "Pending")
        replica = status.get("replica", "")
        cache = status.get("cacheLocation", "")
        msg = status.get("message", "")
        if phase != row.phase and row.phase != "":
            event(phase, f"{row.phase} -> {phase}. {msg}".strip())
        if replica != row.replica and replica and row.replica:
            event("Moved", f"{row.replica} -> {replica}")
        if cache != row.cache_location and cache == "tier":
            event("KVOffloaded", msg)
        if cache != row.cache_location and cache == "none" and row.cache_location:
            event("KVLost", msg)
        if deleted and not row.deleted:
            event("Deleted")

        row.team, row.model = team, spec.get("model", "")
        row.priority, row.desired = spec.get("priority", 0), spec.get("desiredState", "Active")
        row.phase, row.node, row.replica = phase, status.get("node", ""), replica
        row.cache_location, row.cached_tokens = cache, status.get("cachedTokens", 0)
        row.message = msg
        row.hibernations, row.rescues = status.get("hibernations", 0), status.get("rescues", 0)
        row.resource_version, row.deleted, row.updated_at = rv, deleted, now()
        db.commit()


def relist() -> str:
    """Full repair pass: record everything in the cluster, and mark rows whose
    resource no longer exists as deleted. Returns the list's resourceVersion."""
    listing = kube.list_sessions()
    live = set()
    for obj in listing.get("items", []):
        live.add(obj["metadata"]["name"])
        apply(obj)
    with SessionLocal() as db:
        for row in db.query(SessionRow).filter(SessionRow.deleted.is_(False)):
            if row.name not in live:
                row.deleted, row.phase, row.updated_at = True, "Deleted", now()
                db.add(LifecycleEvent(session=row.name, team=row.team, kind="Deleted", detail="found missing during relist"))
        db.commit()
    return listing["metadata"]["resourceVersion"]


def run_forever() -> None:
    while True:
        try:
            rv = relist()
            for ev in watch.Watch().stream(
                    kube.custom.list_namespaced_custom_object, kube.GROUP, kube.VERSION, kube.NS, kube.PLURAL,
                    resource_version=rv, timeout_seconds=300):
                apply(ev["object"], deleted=ev["type"] == "DELETED")
        except Exception as e:  # watch expired or API hiccup: relist and resume
            log.warning("watch restarted: %s", e)
            time.sleep(2)


def start_in_background() -> None:
    threading.Thread(target=run_forever, daemon=True, name="sync-worker").start()


if __name__ == "__main__":
    # Runs as its own Deployment (one replica) so API replicas can scale freely.
    logging.basicConfig(level=logging.INFO)
    run_forever()
