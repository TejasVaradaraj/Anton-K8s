"""Test setup: a fresh SQLite database built by the Alembic migrations, and a
fake Kubernetes that turns API calls into AgentSession objects and feeds them
through the real sync worker, the same path the cluster takes."""
import os
import tempfile

_db = tempfile.NamedTemporaryFile(suffix=".db", delete=False)
os.environ["DATABASE_URL"] = f"sqlite:///{_db.name}"
os.environ["RUN_SYNC"] = "0"
os.environ["DEV_LOGIN"] = "1"
os.environ["CHAT_WAKE_TIMEOUT"] = "1"
os.environ.pop("REDIS_URL", None)

import pytest  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

from app import db as dbmod  # noqa: E402
from app import kube, main, sync  # noqa: E402
from app.ratelimit import limiter  # noqa: E402


class FakeKube:
    def __init__(self):
        self.objects: dict[str, dict] = {}
        self.rv = 0
        self.benches: dict[str, tuple[str, list]] = {}
        self.calls: list[tuple] = []

    def _bump(self, name, deleted=False):
        self.rv += 1
        obj = self.objects[name]
        obj["metadata"]["resourceVersion"] = str(self.rv)
        sync.apply(obj, deleted=deleted)

    def create_session(self, name, team, model, priority, idle):
        self.calls.append(("create", name, model, priority, idle))
        self.objects[name] = {"metadata": {"name": name},
                              "spec": {"team": team, "model": model, "priority": priority,
                                       "idleTimeoutSeconds": idle, "desiredState": "Active"},
                              "status": {"phase": "Pending"}}
        self._bump(name)

    def set_status(self, name, **status):
        self.objects[name]["status"].update(status)
        self._bump(name)

    def set_desired(self, name, state):
        self.calls.append(("desired", name, state))
        self.objects[name]["spec"]["desiredState"] = state
        self._bump(name)

    def delete_session(self, name):
        self.calls.append(("delete", name))
        self._bump(name, deleted=True)
        del self.objects[name]

    def list_sessions(self):
        return {"items": list(self.objects.values()), "metadata": {"resourceVersion": str(self.rv)}}

    def sandbox_ip(self, name):
        return None

    def sandbox_ready(self, name):
        return None

    def ops_snapshot(self):
        return {"nodes": [], "replicas": [], "mode": "anton"}

    def set_mode(self, mode):
        self.calls.append(("mode", mode))

    def launch_bench(self, job, duration, sessions, seeds="1", spot_deadline="10s"):
        self.calls.append(("bench", job, duration, sessions, seeds, spot_deadline))
        self.benches[job] = ("running", [])

    def bench_status(self, job):
        return self.benches.get(job, ("failed", []))


@pytest.fixture(scope="session", autouse=True)
def schema():
    dbmod.migrate()
    yield
    os.unlink(_db.name)


@pytest.fixture(autouse=True)
def clean(monkeypatch):
    with dbmod.SessionLocal() as db:
        for table in reversed(dbmod.Base.metadata.sorted_tables):
            if table.name != "templates":
                db.execute(table.delete())
        db.execute(dbmod.Template.__table__.delete().where(dbmod.Template.team_id.is_not(None)))
        db.commit()
    limiter._local.clear()
    fake = FakeKube()
    for fn in ["create_session", "set_desired", "delete_session", "list_sessions", "sandbox_ip", "sandbox_ready", "ops_snapshot",
               "set_mode", "launch_bench", "bench_status"]:
        monkeypatch.setattr(kube, fn, getattr(fake, fn))
    yield fake


@pytest.fixture
def fake(clean):
    return clean


@pytest.fixture
def client():
    return TestClient(main.app)


def login(client, name, team):
    r = client.post("/api/login", json={"name": name, "team": team})
    assert r.status_code == 200, r.text
    return {"Authorization": f"Bearer {r.json()['token']}"}
