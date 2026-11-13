import time

import pytest
from conftest import login

from app import db as dbmod
from app import sync
from app.db import ApiKey, AuditEntry, BenchmarkSample, LifecycleEvent, Quota, SessionLocal
from app.ratelimit import Limiter


def test_migrations_seed_builtin_templates(client):
    h = login(client, "ana", "core")
    names = [t["name"] for t in client.get("/api/templates", headers=h).json()["templates"]]
    assert {"agent", "background", "priority"} <= set(names)


def test_first_user_owns_team_and_later_users_are_members(client):
    assert client.post("/api/login", json={"name": "ana", "team": "core"}).json()["role"] == "owner"
    assert client.post("/api/login", json={"name": "bo", "team": "core"}).json()["role"] == "member"


def test_keys_are_stored_hashed(client):
    token = client.post("/api/login", json={"name": "ana", "team": "core"}).json()["token"]
    with SessionLocal() as db:
        stored = [k.hash for k in db.query(ApiKey)]
    assert token not in stored and len(stored) == 1 and len(stored[0]) == 64


def test_requests_need_a_valid_key(client):
    assert client.get("/api/sessions").status_code == 401
    assert client.get("/api/sessions", headers={"Authorization": "Bearer nope"}).status_code == 401


def test_create_session_goes_through_the_cluster_and_sync(client, fake):
    h = login(client, "ana", "core")
    name = client.post("/api/sessions", json={"label": "fix", "template": "background"}, headers=h).json()["name"]
    assert fake.calls[0] == ("create", name, "mock", 0, 600)  # template's idle timeout reached the CRD
    rows = client.get("/api/sessions", headers=h).json()["sessions"]
    assert [r["name"] for r in rows] == [name] and rows[0]["owner"] == "ana" and rows[0]["template_id"]


def test_explicit_values_override_the_template(client, fake):
    h = login(client, "ana", "core")
    client.post("/api/sessions", json={"template": "background", "idle_timeout_seconds": 30, "priority": 3}, headers=h)
    assert fake.calls[0][3:] == (3, 30)


def test_teams_never_see_each_other(client, fake):
    dev = login(client, "ana", "dev")
    ops = login(client, "bo", "dev-ops")
    name = client.post("/api/sessions", json={}, headers=ops).json()["name"]
    assert name.startswith("dev-")  # the prefix trap the WebSocket used to fall into
    assert client.get("/api/sessions", headers=dev).json()["sessions"] == []
    assert client.get(f"/api/sessions/{name}", headers=dev).status_code == 404
    assert client.post(f"/api/sessions/{name}/hibernate", headers=dev).status_code == 404
    assert client.delete(f"/api/sessions/{name}", headers=dev).status_code == 404


def test_websocket_only_streams_own_team_events(client, fake):
    dev = login(client, "ana", "dev")
    ops = login(client, "bo", "dev-ops")
    token = dev["Authorization"].split()[1]
    with client.websocket_connect(f"/ws?token={token}") as ws:
        other = client.post("/api/sessions", json={}, headers=ops).json()["name"]
        mine = client.post("/api/sessions", json={}, headers=dev).json()["name"]
        assert other.startswith("dev-")
        msg = ws.receive_json()
        assert msg["session"] == mine and msg["kind"] == "Created"


def test_websocket_rejects_bad_token(client):
    from starlette.websockets import WebSocketDisconnect
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect("/ws?token=bad") as ws:
            ws.receive_json()


def set_quota(team_name, **values):
    with SessionLocal() as db:
        team = db.query(dbmod.Team).filter(dbmod.Team.name == team_name).one()
        q = db.get(Quota, team.id)
        for k, v in values.items():
            setattr(q, k, v)
        db.commit()


def test_session_quota(client):
    h = login(client, "ana", "core")
    set_quota("core", max_sessions=2, max_active_sessions=5)
    assert client.post("/api/sessions", json={}, headers=h).status_code == 201
    assert client.post("/api/sessions", json={}, headers=h).status_code == 201
    r = client.post("/api/sessions", json={}, headers=h)
    assert r.status_code == 429 and "quota" in r.json()["detail"]


def test_hibernated_sessions_free_active_quota(client):
    h = login(client, "ana", "core")
    set_quota("core", max_sessions=5, max_active_sessions=1)
    first = client.post("/api/sessions", json={}, headers=h).json()["name"]
    assert client.post("/api/sessions", json={}, headers=h).status_code == 429
    assert client.post(f"/api/sessions/{first}/hibernate", headers=h).status_code == 200
    second = client.post("/api/sessions", json={}, headers=h).json()["name"]
    # Waking the first would exceed the active limit while the second runs.
    assert client.post(f"/api/sessions/{first}/resume", headers=h).status_code == 429
    client.post(f"/api/sessions/{second}/hibernate", headers=h)
    assert client.post(f"/api/sessions/{first}/resume", headers=h).status_code == 200


def test_templates_are_owner_managed_and_team_scoped(client, fake):
    owner = login(client, "ana", "core")
    member = login(client, "bo", "core")
    other = login(client, "cy", "else")
    body = {"name": "nightly", "idle_timeout_seconds": 120, "priority": 2}
    assert client.post("/api/templates", json=body, headers=member).status_code == 403
    assert client.post("/api/templates", json=body, headers=owner).status_code == 201
    assert client.post("/api/templates", json=body, headers=owner).status_code == 409
    assert "nightly" not in [t["name"] for t in client.get("/api/templates", headers=other).json()["templates"]]
    assert client.post("/api/sessions", json={"template": "nightly"}, headers=other).status_code == 404
    client.post("/api/sessions", json={"template": "nightly"}, headers=member)
    assert fake.calls[-1][3:] == (2, 120)


def test_api_keys_create_use_revoke(client):
    h = login(client, "ana", "core")
    key = client.post("/api/team/keys", json={"name": "ci"}, headers=h).json()["key"]
    kh = {"Authorization": f"Bearer {key}"}
    assert client.get("/api/sessions", headers=kh).status_code == 200
    listed = client.get("/api/team/keys", headers=h).json()["keys"]
    assert [k["name"] for k in listed] == ["ci"] and key.startswith(listed[0]["prefix"])
    assert client.delete(f"/api/team/keys/{listed[0]['id']}", headers=h).status_code == 200
    assert client.get("/api/sessions", headers=kh).status_code == 401


def test_logout_revokes_the_login_key(client):
    h = login(client, "ana", "core")
    assert client.post("/api/logout", headers=h).status_code == 200
    assert client.get("/api/me", headers=h).status_code == 401


def test_members_invite_and_remove(client):
    owner = login(client, "ana", "core")
    member = login(client, "cy", "core")
    login(client, "bo", "else")  # bo exists, but in another team
    assert client.post("/api/team/members", json={"user": "bo"}, headers=member).status_code == 403
    r = client.post("/api/team/members", json={"user": "bo"}, headers=owner)
    assert r.status_code == 201
    team = client.get("/api/team", headers=owner).json()
    assert {m["name"] for m in team["members"]} == {"ana", "bo", "cy"}
    me = [m for m in team["members"] if m["name"] == "ana"][0]
    assert client.delete(f"/api/team/members/{me['id']}", headers=owner).status_code == 409  # last owner
    assert client.delete(f"/api/team/members/{r.json()['id']}", headers=owner).status_code == 200


def test_removed_member_loses_access(client):
    owner = login(client, "ana", "core")
    bo = login(client, "bo", "core")
    bo_id = [m["id"] for m in client.get("/api/team", headers=owner).json()["members"] if m["name"] == "bo"][0]
    client.delete(f"/api/team/members/{bo_id}", headers=owner)
    assert client.get("/api/sessions", headers=bo).status_code == 401


def test_invite_by_email_precreates_user(client):
    owner = login(client, "ana", "core")
    r = client.post("/api/team/members", json={"user": "Dee@Example.com"}, headers=owner)
    assert r.status_code == 201 and r.json()["name"] == "dee"
    with SessionLocal() as db:
        assert db.query(dbmod.User).filter(dbmod.User.email == "dee@example.com").count() == 1


def test_audit_log_records_actions_for_owners_only(client):
    owner = login(client, "ana", "core")
    member = login(client, "bo", "core")
    name = client.post("/api/sessions", json={}, headers=member).json()["name"]
    client.post(f"/api/sessions/{name}/hibernate", headers=member)
    assert client.get("/api/audit", headers=member).status_code == 403
    entries = client.get("/api/audit", headers=owner).json()["entries"]
    actions = [(e["actor"], e["action"]) for e in entries]
    assert ("bo", "session.create") in actions and ("bo", "session.hibernate") in actions
    other = login(client, "cy", "else")
    assert all(e["actor"] != "cy" for e in client.get("/api/audit", headers=owner).json()["entries"])
    with SessionLocal() as db:
        assert db.query(AuditEntry).filter(AuditEntry.actor == "cy").count() == 1  # cy's own login
    assert other


def test_rate_limit(client):
    h = login(client, "ana", "core")
    set_quota("core", requests_per_minute=3)
    codes = [client.get("/api/me", headers=h).status_code for _ in range(4)]
    assert codes[:3] == [200, 200, 200] and codes[3] == 429


def test_limiter_window_resets():
    lim = Limiter(url="")
    t = 1_000_000 * 60.0
    assert lim.hit("k", 1, now=t)[0] and not lim.hit("k", 1, now=t + 1)[0]
    assert lim.hit("k", 1, now=t + 60)[0]


def test_limiter_fails_open_without_redis():
    lim = Limiter(url="redis://127.0.0.1:1/0")
    assert lim.hit("k", 0)[0]


def test_mode_change_is_owner_only(client, fake):
    owner = login(client, "ana", "core")
    member = login(client, "bo", "core")
    assert client.put("/api/mode", json={"mode": "plain"}, headers=member).status_code == 403
    assert client.put("/api/mode", json={"mode": "plain"}, headers=owner).status_code == 200
    assert ("mode", "plain") in fake.calls


def test_benchmark_results_flatten_into_samples(client, fake):
    h = login(client, "ana", "core")
    job = client.post("/api/benchmarks", json={"duration": "1m"}, headers=h).json()["job"]
    assert client.post("/api/benchmarks", json={}, headers=h).status_code == 409  # one at a time
    result = {"mode": "anton", "seed": 1, "requests": 10, "errors": 0, "lost_kv_state": 1, "prefill_gpu_seconds": 4.5}
    fake.benches[job] = ("running", [result])
    client.get("/api/benchmarks", headers=h)
    fake.benches[job] = ("succeeded", [result])
    runs = client.get("/api/benchmarks", headers=h).json()["runs"]
    assert runs[0]["status"] == "succeeded" and runs[0]["launched_by"] == "ana"
    with SessionLocal() as db:
        assert db.query(BenchmarkSample).count() == 4  # stored once despite two refreshes ("seed" is not a metric)
    series = client.get("/api/benchmarks/samples?metric=lost_kv_state", headers=h).json()["samples"]
    assert series == [{"job": job, "at": series[0]["at"], "mode": "anton", "value": 1.0}]


def test_benchmark_daily_limit(client, fake):
    h = login(client, "ana", "core")
    set_quota("core", max_benchmarks_per_day=1)
    job = client.post("/api/benchmarks", json={}, headers=h).json()["job"]
    fake.benches[job] = ("succeeded", [])
    client.get("/api/benchmarks", headers=h)
    time.sleep(1.1)  # job names are per second
    assert client.post("/api/benchmarks", json={}, headers=h).status_code == 429


def test_sync_is_idempotent_and_records_transitions(client, fake):
    h = login(client, "ana", "core")
    name = client.post("/api/sessions", json={}, headers=h).json()["name"]
    obj = fake.objects[name]
    sync.apply(obj)  # replaying the same resourceVersion changes nothing
    fake.set_status(name, phase="Active", replica="r1", cacheLocation="replica")
    fake.set_status(name, replica="r2")
    fake.set_status(name, phase="Hibernated", cacheLocation="tier", message="offloaded")
    with SessionLocal() as db:
        kinds = [e.kind for e in db.query(LifecycleEvent).filter(LifecycleEvent.session == name).order_by(LifecycleEvent.id)]
        teams = {e.team for e in db.query(LifecycleEvent)}
    assert kinds == ["Created", "Active", "Moved", "Hibernated", "KVOffloaded"]
    assert teams == {"core"}


def test_relist_marks_vanished_sessions_deleted(client, fake):
    h = login(client, "ana", "core")
    name = client.post("/api/sessions", json={}, headers=h).json()["name"]
    del fake.objects[name]  # gone from the cluster while the worker was down
    sync.relist()
    assert client.get("/api/sessions", headers=h).json()["sessions"] == []


def test_metrics_endpoint(client):
    h = login(client, "ana", "core")
    client.get("/api/sessions", headers=h)
    body = client.get("/metrics").text
    assert 'anton_api_requests_total{method="GET",route="/api/sessions",status="200"}' in body


def test_files_need_a_running_sandbox_and_respect_tenancy(client, fake):
    h = login(client, "ana", "core")
    other = login(client, "bo", "else")
    name = client.post("/api/sessions", json={}, headers=h).json()["name"]
    assert client.get(f"/api/sessions/{name}/files", headers=h).status_code == 409  # FakeKube: no pod IP
    assert client.get(f"/api/sessions/{name}/files", headers=other).status_code == 404
    assert client.get(f"/api/sessions/{name}/files/content?path=a", headers=other).status_code == 404


def test_terminal_rejects_other_teams_and_bad_tokens(client, fake):
    from starlette.websockets import WebSocketDisconnect
    h = login(client, "ana", "core")
    other = login(client, "bo", "else")
    name = client.post("/api/sessions", json={}, headers=h).json()["name"]
    for token in ["bad", other["Authorization"].split()[1]]:
        with pytest.raises(WebSocketDisconnect) as e:
            with client.websocket_connect(f"/api/sessions/{name}/terminal?token={token}") as ws:
                ws.receive_text()
        assert e.value.code == 4404
    with pytest.raises(WebSocketDisconnect) as e:  # own session, but no sandbox running
        with client.websocket_connect(f"/api/sessions/{name}/terminal?token={h['Authorization'].split()[1]}") as ws:
            ws.receive_text()
    assert e.value.code == 4409


def test_seeds_combine_into_one_result_per_mode():
    from app.main import combine_by_mode
    out = combine_by_mode([
        {"mode": "plain", "seed": 1, "requests": 10, "errors": 0, "lost_kv_state": 2, "prefill_gpu_seconds": 20, "ttft_p50_ms": 100},
        {"mode": "anton", "seed": 1, "requests": 10, "errors": 0, "lost_kv_state": 0, "prefill_gpu_seconds": 5, "ttft_p50_ms": 50},
        {"mode": "plain", "seed": 2, "requests": 30, "errors": 0, "lost_kv_state": 4, "prefill_gpu_seconds": 30, "ttft_p50_ms": 300},
    ])
    assert [r["mode"] for r in out] == ["plain", "anton"]
    plain = out[0]
    assert plain["seeds"] == 2 and plain["requests"] == 40 and plain["lost_kv_state"] == 6
    assert plain["ttft_p50_ms"] == 200 and plain["gpu_s_per_request_by_seed"] == [2.0, 1.0]
    assert "seed" not in plain


def test_benchmark_options_reach_the_job(client, fake):
    h = login(client, "ana", "core")
    client.post("/api/benchmarks", json={"duration": "1m", "seeds": 3, "spot_deadline": "120s"}, headers=h)
    assert fake.calls[-1][2:] == ("1m", 24, "1,2,3", "120s")
    assert client.post("/api/benchmarks", json={"spot_deadline": "7s"}, headers=h).status_code == 422


def test_created_event_when_api_inserted_the_row_first(client, fake):
    from app.db import SessionRow
    h = login(client, "ana", "core")
    with SessionLocal() as db:  # the API won the race: row exists, never synced
        db.add(SessionRow(name="core-x-0001", team="core", owner="ana"))
        db.commit()
    sync.apply({"metadata": {"name": "core-x-0001", "resourceVersion": "7"},
                "spec": {"team": "core"}, "status": {"phase": "Pending"}})
    with SessionLocal() as db:
        kinds = [e.kind for e in db.query(LifecycleEvent).filter(LifecycleEvent.session == "core-x-0001")]
        owner = db.get(SessionRow, "core-x-0001").owner
    assert kinds == ["Created"] and owner == "ana"


def test_chat_wakes_a_hibernated_session_then_times_out_without_a_sandbox(client, fake):
    h = login(client, "ana", "core")
    name = client.post("/api/sessions", json={}, headers=h).json()["name"]
    client.post(f"/api/sessions/{name}/hibernate", headers=h)
    r = client.post(f"/api/sessions/{name}/messages", json={"content": "hi"}, headers=h)
    assert r.status_code == 504  # FakeKube never reports a ready sandbox
    assert ("desired", name, "Active") in fake.calls  # but the message did wake the session
    entries = [e["action"] for e in client.get("/api/audit", headers=h).json()["entries"]]
    assert "session.resume" in entries
