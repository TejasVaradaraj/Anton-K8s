"""Anton product API: auth, teams, sessions, chat, ops view, benchmarks, live events."""
import asyncio
import json
import os
from contextlib import asynccontextmanager
import re
import secrets
from datetime import timedelta
from pathlib import Path
from urllib.parse import urlencode

import httpx
from fastapi import Depends, FastAPI, HTTPException, Query, WebSocket, WebSocketDisconnect
from fastapi.responses import RedirectResponse
from fastapi.staticfiles import StaticFiles
from kubernetes.client import ApiException
from pydantic import BaseModel, Field
from sqlalchemy.exc import IntegrityError

from . import auth, kube, metrics, sync
from .auth import Principal, current, owner
from .db import (ApiKey, AuditEntry, BenchmarkRun, BenchmarkSample, LifecycleEvent, Membership, Message, Quota,
                 SessionLocal, SessionRow, Team, Template, User, migrate, now)

ROUTER_URL = os.environ.get("ROUTER_URL", "http://anton-router:8080")


@asynccontextmanager
async def lifespan(_: FastAPI):
    # In the cluster an init container runs migrations and the sync worker is
    # its own Deployment; both default on so `uvicorn app.main:app` just works.
    if os.environ.get("RUN_MIGRATIONS", "1") == "1":
        migrate()
    if os.environ.get("RUN_SYNC", "1") == "1":
        sync.start_in_background()
    yield


app = FastAPI(title="Anton Platform", lifespan=lifespan)
metrics.instrument(app)


def audit(p: Principal | None, action: str, target: str = "", **detail) -> None:
    with SessionLocal() as db:
        db.add(AuditEntry(team_id=p.team_id if p else None, user_id=p.user_id if p else None,
                          actor=p.user if p else "", action=action, target=target, detail=detail))
        db.commit()


# ---------- auth ----------

NAME = r"^[a-z0-9-]{2,30}$"


class LoginIn(BaseModel):
    name: str = Field(pattern=NAME)
    team: str = Field(pattern=NAME)


@app.get("/api/auth/config")
def auth_config():
    return {"google": auth.GOOGLE_ENABLED, "dev_login": auth.DEV_LOGIN}


@app.post("/api/login")
def dev_login(body: LoginIn):
    """Development login: a name and a team, no password. Off when DEV_LOGIN=0."""
    if not auth.DEV_LOGIN:
        raise HTTPException(404, "dev login is disabled")
    with SessionLocal() as db:
        user = db.query(User).filter(User.name == body.name).first()
        if user is None:
            user = User(name=body.name)
            db.add(user)
            db.flush()
        team, m = auth.join_team(db, user, body.team, allow_open_join=True)
        key = auth.issue_key(db, user, team)
        db.commit()
        p = Principal(user.id, user.name, team.id, team.name, m.role, 0)
    audit(p, "login", team.name, method="dev")
    return {"token": key, "name": p.user, "team": p.team, "role": p.role}


@app.get("/api/auth/google/start")
def google_start(team: str = Query(pattern=NAME)):
    if not auth.GOOGLE_ENABLED:
        raise HTTPException(404, "Google login is not configured")
    return RedirectResponse(auth.google_start_url(team))


@app.get("/api/auth/google/callback")
def google_callback(code: str = "", state: str = "", error: str = ""):
    if error or not code:
        return RedirectResponse("/#" + urlencode({"login_error": error or "cancelled"}))
    try:
        key, name, team = auth.google_finish(code, state)
    except HTTPException as e:
        return RedirectResponse("/#" + urlencode({"login_error": e.detail}))
    # The key travels in the URL fragment, which browsers never send to a server.
    return RedirectResponse("/#" + urlencode({"token": key, "name": name, "team": team}))


@app.post("/api/logout")
def logout(p: Principal = Depends(current)):
    with SessionLocal() as db:
        db.get(ApiKey, p.key_id).revoked = True
        db.commit()
    return {"ok": True}


@app.get("/api/me")
def me(p: Principal = Depends(current)):
    with SessionLocal() as db:
        teams = db.query(Team.name, Membership.role).join(Membership, Membership.team_id == Team.id) \
            .filter(Membership.user_id == p.user_id).order_by(Team.name).all()
    return {"name": p.user, "team": p.team, "role": p.role, "teams": [{"name": t, "role": r} for t, r in teams]}


# ---------- team, keys, templates, audit ----------

def quota_of(db, team_id: int) -> Quota:
    q = db.get(Quota, team_id)
    if q is None:
        q = Quota(team_id=team_id)
        db.add(q)
        db.flush()
    return q


def usage(db, team: str) -> dict:
    live = db.query(SessionRow).filter(SessionRow.team == team, SessionRow.deleted.is_(False))
    return {"sessions": live.count(), "active_sessions": live.filter(SessionRow.desired == "Active").count()}


@app.get("/api/team")
def team_detail(p: Principal = Depends(current)):
    with SessionLocal() as db:
        members = db.query(User, Membership.role).join(Membership, Membership.user_id == User.id) \
            .filter(Membership.team_id == p.team_id).order_by(User.name).all()
        q = quota_of(db, p.team_id)
        today = now().replace(hour=0, minute=0, second=0, microsecond=0)
        benches = db.query(BenchmarkRun).filter(BenchmarkRun.team_id == p.team_id, BenchmarkRun.started_at >= today).count()
        out = {
            "name": p.team, "role": p.role,
            "members": [{"id": u.id, "name": u.name, "email": u.email, "role": r} for u, r in members],
            "quota": {"max_sessions": q.max_sessions, "max_active_sessions": q.max_active_sessions,
                      "max_benchmarks_per_day": q.max_benchmarks_per_day, "requests_per_minute": q.requests_per_minute},
            "usage": {**usage(db, p.team), "benchmarks_today": benches},
        }
        db.commit()
    return out


class InviteIn(BaseModel):
    user: str = Field(min_length=2, max_length=254, description="user name, or an email for Google login")
    role: str = Field(default="member", pattern=r"^(member|owner)$")


@app.post("/api/team/members", status_code=201)
def invite(body: InviteIn, p: Principal = Depends(owner)):
    with SessionLocal() as db:
        if "@" in body.user:
            email = body.user.lower()
            user = db.query(User).filter(User.email == email).first()
            if user is None:  # pre-create so their first Google login lands in this team
                base = re.sub(r"[^a-z0-9-]", "-", email.split("@")[0])[:24].strip("-") or "user"
                name = base if not db.query(User).filter(User.name == base).first() else f"{base}-{secrets.token_hex(2)}"
                user = User(name=name, email=email)
                db.add(user)
                db.flush()
        else:
            user = db.query(User).filter(User.name == body.user).first()
            if user is None:
                raise HTTPException(404, "no such user")
        m = db.get(Membership, (user.id, p.team_id))
        if m is None:
            db.add(Membership(user_id=user.id, team_id=p.team_id, role=body.role))
        else:
            m.role = body.role
        db.commit()
        uid, uname = user.id, user.name
    audit(p, "member.add", uname, role=body.role)
    return {"id": uid, "name": uname, "role": body.role}


@app.delete("/api/team/members/{user_id}")
def remove_member(user_id: int, p: Principal = Depends(owner)):
    with SessionLocal() as db:
        m = db.get(Membership, (user_id, p.team_id))
        if m is None:
            raise HTTPException(404, "not a member")
        owners = db.query(Membership).filter(Membership.team_id == p.team_id, Membership.role == "owner").count()
        if m.role == "owner" and owners == 1:
            raise HTTPException(409, "a team needs at least one owner")
        db.delete(m)
        db.query(ApiKey).filter(ApiKey.user_id == user_id, ApiKey.team_id == p.team_id).update({"revoked": True})
        db.commit()
    audit(p, "member.remove", str(user_id))
    return {"ok": True}


class KeyIn(BaseModel):
    name: str = Field(min_length=1, max_length=63)


def key_json(k: ApiKey) -> dict:
    return {"id": k.id, "name": k.name, "kind": k.kind, "prefix": k.prefix, "created_at": k.created_at,
            "last_used_at": k.last_used_at}


@app.get("/api/team/keys")
def list_keys(p: Principal = Depends(current)):
    with SessionLocal() as db:
        keys = db.query(ApiKey).filter(ApiKey.user_id == p.user_id, ApiKey.team_id == p.team_id,
                                       ApiKey.kind == "api", ApiKey.revoked.is_(False)).order_by(ApiKey.id).all()
    return {"keys": [key_json(k) for k in keys]}


@app.post("/api/team/keys", status_code=201)
def create_key(body: KeyIn, p: Principal = Depends(current)):
    with SessionLocal() as db:
        key = auth.issue_key(db, db.get(User, p.user_id), db.get(Team, p.team_id), kind="api", name=body.name)
        db.commit()
    audit(p, "key.create", body.name)
    return {"key": key, "note": "shown once; store it now"}


@app.delete("/api/team/keys/{key_id}")
def revoke_key(key_id: int, p: Principal = Depends(current)):
    with SessionLocal() as db:
        k = db.get(ApiKey, key_id)
        if k is None or k.user_id != p.user_id or k.team_id != p.team_id:
            raise HTTPException(404, "no such key")
        k.revoked = True
        db.commit()
    audit(p, "key.revoke", str(key_id))
    return {"ok": True}


class TemplateIn(BaseModel):
    name: str = Field(pattern=r"^[a-z0-9-]{1,20}$")
    description: str = Field(default="", max_length=500)
    priority: int = Field(default=0, ge=0, le=10)
    idle_timeout_seconds: int = Field(default=0, ge=0, le=86400)


def template_json(t: Template) -> dict:
    return {"id": t.id, "name": t.name, "description": t.description, "model": t.model, "priority": t.priority,
            "idle_timeout_seconds": t.idle_timeout_seconds, "builtin": t.team_id is None}


def visible_templates(db, team_id: int):
    return db.query(Template).filter((Template.team_id.is_(None)) | (Template.team_id == team_id))


@app.get("/api/templates")
def list_templates(p: Principal = Depends(current)):
    with SessionLocal() as db:
        rows = visible_templates(db, p.team_id).order_by(Template.team_id.is_(None).desc(), Template.name).all()
    return {"templates": [template_json(t) for t in rows]}


@app.post("/api/templates", status_code=201)
def create_template(body: TemplateIn, p: Principal = Depends(owner)):
    with SessionLocal() as db:
        if visible_templates(db, p.team_id).filter(Template.name == body.name).first():
            raise HTTPException(409, "a template with that name exists")
        t = Template(team_id=p.team_id, **body.model_dump())
        db.add(t)
        db.commit()
        out = template_json(t)
    audit(p, "template.create", body.name)
    return out


@app.delete("/api/templates/{template_id}")
def delete_template(template_id: int, p: Principal = Depends(owner)):
    with SessionLocal() as db:
        t = db.get(Template, template_id)
        if t is None or t.team_id != p.team_id:
            raise HTTPException(404, "no such team template")
        db.delete(t)
        db.commit()
    audit(p, "template.delete", str(template_id))
    return {"ok": True}


@app.get("/api/audit")
def audit_log(limit: int = Query(default=100, ge=1, le=500), p: Principal = Depends(owner)):
    with SessionLocal() as db:
        rows = db.query(AuditEntry).filter(AuditEntry.team_id == p.team_id).order_by(AuditEntry.id.desc()).limit(limit).all()
    return {"entries": [{"id": a.id, "at": a.at, "actor": a.actor, "action": a.action, "target": a.target,
                         "detail": a.detail} for a in rows]}


# ---------- sessions ----------

class SessionIn(BaseModel):
    label: str = Field(default="agent", pattern=r"^[a-z0-9-]{1,20}$")
    template: str = Field(default="agent", pattern=r"^[a-z0-9-]{1,20}$")
    priority: int | None = Field(default=None, ge=0, le=10)
    idle_timeout_seconds: int | None = Field(default=None, ge=0, le=86400)


def row_json(r: SessionRow) -> dict:
    return {c.name: getattr(r, c.name) for c in SessionRow.__table__.columns}


def owned(name: str, p: Principal) -> SessionRow:
    with SessionLocal() as db:
        row = db.get(SessionRow, name)
    if row is None or row.deleted or row.team != p.team:
        raise HTTPException(404, "no such session in your team")  # tenancy: teams never see each other
    return row


@app.get("/api/sessions")
def list_sessions(p: Principal = Depends(current)):
    with SessionLocal() as db:
        rows = db.query(SessionRow).filter(SessionRow.team == p.team, SessionRow.deleted.is_(False)) \
            .order_by(SessionRow.created_at.desc()).all()
        q = quota_of(db, p.team_id)
        u = usage(db, p.team)
        db.commit()
    return {"sessions": [row_json(r) for r in rows], "max_sessions": q.max_sessions,
            "max_active_sessions": q.max_active_sessions, "usage": u}


@app.post("/api/sessions", status_code=201)
def create_session(body: SessionIn, p: Principal = Depends(current)):
    with SessionLocal() as db:
        t = visible_templates(db, p.team_id).filter(Template.name == body.template) \
            .order_by(Template.team_id.is_(None)).first()  # a team template shadows a built-in of the same name
        if t is None:
            raise HTTPException(404, "no such template")
        q, u = quota_of(db, p.team_id), usage(db, p.team)
        db.commit()
        tpl = template_json(t)
    if u["sessions"] >= q.max_sessions:
        raise HTTPException(429, f"team quota reached ({q.max_sessions} sessions)")
    if u["active_sessions"] >= q.max_active_sessions:
        raise HTTPException(429, f"team has {q.max_active_sessions} active sessions; hibernate one first")
    priority = tpl["priority"] if body.priority is None else body.priority
    idle = tpl["idle_timeout_seconds"] if body.idle_timeout_seconds is None else body.idle_timeout_seconds
    name = f"{p.team}-{body.label}-{secrets.token_hex(2)}"
    try:  # cluster first; Postgres only records who asked
        kube.create_session(name, p.team, tpl["model"], priority, idle)
    except ApiException as e:
        raise HTTPException(502, f"cluster refused: {e.reason}")
    for _ in range(2):  # the sync worker may insert the same row at the same moment
        try:
            with SessionLocal() as db:
                row = db.get(SessionRow, name) or SessionRow(name=name, team=p.team)
                row.owner, row.template_id = p.user, tpl["id"]
                db.add(row)
                db.commit()
            break
        except IntegrityError:
            continue
    audit(p, "session.create", name, template=tpl["name"], priority=priority, idle_timeout_seconds=idle)
    return {"name": name}


@app.get("/api/sessions/{name}")
def session_detail(name: str, p: Principal = Depends(current)):
    row = owned(name, p)
    with SessionLocal() as db:
        msgs = db.query(Message).filter(Message.session == name).order_by(Message.id).all()
        events = db.query(LifecycleEvent).filter(LifecycleEvent.session == name).order_by(LifecycleEvent.id.desc()).limit(100).all()
    workspace = None
    if ip := kube.sandbox_ip(name):
        try:
            workspace = httpx.get(f"http://{ip}:8080/state", timeout=2).json()
        except httpx.HTTPError:
            pass
    return {
        "session": row_json(row), "workspace": workspace,
        "messages": [{"id": m.id, "role": m.role, "content": m.content, "ttft_ms": m.ttft_ms,
                      "cached_tokens": m.cached_tokens, "recomputed_tokens": m.recomputed_tokens,
                      "replica": m.replica, "at": m.created_at} for m in msgs],
        "events": [{"id": e.id, "kind": e.kind, "detail": e.detail, "at": e.at} for e in events],
    }


def sandbox_get(name: str, path: str, params: dict | None = None) -> dict:
    ip = kube.sandbox_ip(name)
    if not ip:
        raise HTTPException(409, "the sandbox is not running (hibernated or starting)")
    try:
        r = httpx.get(f"http://{ip}:8080{path}", params=params, timeout=5)
    except httpx.HTTPError as e:
        raise HTTPException(502, f"sandbox unreachable: {e}")
    if r.status_code != 200:
        raise HTTPException(r.status_code if r.status_code in (400, 404) else 502, r.text.strip())
    return r.json()


@app.get("/api/sessions/{name}/files")
def files(name: str, p: Principal = Depends(current)):
    owned(name, p)
    return sandbox_get(name, "/files")


@app.get("/api/sessions/{name}/files/content")
def file_content(name: str, path: str = Query(min_length=1, max_length=512), p: Principal = Depends(current)):
    owned(name, p)
    return sandbox_get(name, "/files/content", {"path": path})


@app.websocket("/api/sessions/{name}/terminal")
async def terminal(ws: WebSocket, name: str, token: str = ""):
    """A shell in the session's sandbox, bridged to the browser. Client sends
    {"type":"input","data":...} or {"type":"resize","cols":..,"rows":..};
    the server sends raw terminal output as text frames."""
    p = auth.principal_from_token(token)
    with SessionLocal() as db:
        row = db.get(SessionRow, name)
    if p is None or row is None or row.deleted or row.team != p.team:
        await ws.close(code=4404)
        return
    if not kube.sandbox_ip(name):
        await ws.close(code=4409, reason="sandbox not running")
        return
    await ws.accept()
    audit(p, "terminal.open", name)
    try:
        shell = await asyncio.to_thread(kube.exec_shell, name)
    except ApiException as e:
        await ws.send_text(f"\r\n[could not open a shell: {e.reason}]\r\n")
        await ws.close()
        return

    def read_some() -> str:
        shell.update(timeout=0.25)
        out = ""
        if shell.peek_stdout():
            out += shell.read_stdout()
        if shell.peek_stderr():
            out += shell.read_stderr()
        return out

    async def pump() -> None:
        while shell.is_open():
            if data := await asyncio.to_thread(read_some):
                await ws.send_text(data)
        await ws.send_text("\r\n[shell closed]\r\n")
        await ws.close()

    task = asyncio.create_task(pump())
    try:
        while True:
            msg = await ws.receive_json()
            if msg.get("type") == "input" and isinstance(msg.get("data"), str):
                await asyncio.to_thread(shell.write_stdin, msg["data"])
            elif msg.get("type") == "resize":
                size = {"Width": int(msg.get("cols", 80)), "Height": int(msg.get("rows", 24))}
                await asyncio.to_thread(shell.write_channel, 4, json.dumps(size))
    except (WebSocketDisconnect, RuntimeError, ValueError):
        pass
    finally:
        task.cancel()
        shell.close()


@app.post("/api/sessions/{name}/hibernate")
def hibernate(name: str, p: Principal = Depends(current)):
    owned(name, p)
    kube.set_desired(name, "Hibernated")
    audit(p, "session.hibernate", name)
    return {"ok": True}


@app.post("/api/sessions/{name}/resume")
def resume(name: str, p: Principal = Depends(current)):
    row = owned(name, p)
    if row.desired != "Active":
        with SessionLocal() as db:
            q, u = quota_of(db, p.team_id), usage(db, p.team)
            db.commit()
        if u["active_sessions"] >= q.max_active_sessions:
            raise HTTPException(429, f"team has {q.max_active_sessions} active sessions; hibernate one first")
    kube.set_desired(name, "Active")
    audit(p, "session.resume", name)
    return {"ok": True}


@app.delete("/api/sessions/{name}")
def delete(name: str, p: Principal = Depends(current)):
    owned(name, p)
    try:
        kube.delete_session(name)
    except ApiException as e:
        if e.status != 404:
            raise HTTPException(502, e.reason)
    audit(p, "session.delete", name)
    return {"ok": True}


class ChatIn(BaseModel):
    content: str = Field(min_length=1, max_length=20000)


CHAT_WAKE_TIMEOUT = float(os.environ.get("CHAT_WAKE_TIMEOUT", "180"))


@app.post("/api/sessions/{name}/messages")
async def chat(name: str, body: ChatIn, p: Principal = Depends(current)):
    """A turn of the session's agent. The message goes to the agent in the
    sandbox, which keeps the conversation in its workspace and calls the
    session router; a hibernated session is woken first, and the reported
    time includes that wake."""
    row = owned(name, p)
    t0 = asyncio.get_event_loop().time()
    ip = kube.sandbox_ready(name)
    if ip is None:
        if row.desired != "Active":
            with SessionLocal() as db:
                q, u = quota_of(db, p.team_id), usage(db, p.team)
                db.commit()
            if u["active_sessions"] >= q.max_active_sessions:
                raise HTTPException(429, f"team has {q.max_active_sessions} active sessions; hibernate one first")
            kube.set_desired(name, "Active")
            audit(p, "session.resume", name, reason="message")
        while ip is None:
            if asyncio.get_event_loop().time() - t0 > CHAT_WAKE_TIMEOUT:
                raise HTTPException(504, "the session's sandbox did not become ready in time")
            await asyncio.sleep(0.5)
            ip = kube.sandbox_ready(name)
    async with httpx.AsyncClient(timeout=600) as hc:
        try:
            r = await hc.post(f"http://{ip}:8080/chat", json={"content": body.content})
            r.raise_for_status()
        except httpx.HTTPStatusError as e:
            raise HTTPException(502, f"agent error: {e.response.text[:300]}")
        except httpx.HTTPError as e:
            raise HTTPException(502, f"agent unreachable: {e}")
    ttft = (asyncio.get_event_loop().time() - t0) * 1000
    out = r.json()
    cached, recomputed = out.get("cached_tokens", 0), out.get("recomputed_tokens", 0)
    metrics.observe_chat(ttft / 1000, cached, recomputed)
    with SessionLocal() as db:
        db.add(Message(session=name, role="user", content=body.content))
        db.add(Message(session=name, role="assistant", content=out.get("reply", ""), ttft_ms=ttft,
                       cached_tokens=cached, recomputed_tokens=recomputed, replica=out.get("replica") or None))
        fresh = db.get(SessionRow, name)
        fresh.context_tokens = out.get("prompt_tokens") or fresh.context_tokens
        db.commit()
    return {**out, "ttft_ms": ttft}


# ---------- ops ----------

@app.get("/api/ops")
def ops(p: Principal = Depends(current)):
    snap = kube.ops_snapshot()
    with SessionLocal() as db:
        rows = db.query(SessionRow).filter(SessionRow.deleted.is_(False)).all()
    phases: dict[str, int] = {}
    per_node: dict[str, int] = {}
    per_replica: dict[str, int] = {}
    for r in rows:
        phases[r.phase] = phases.get(r.phase, 0) + 1
        if r.node:
            per_node[r.node] = per_node.get(r.node, 0) + 1
        if r.replica:
            per_replica[r.replica] = per_replica.get(r.replica, 0) + 1
    for n in snap["nodes"]:
        n["sandboxes"] = per_node.get(n["name"], 0)
    for rp in snap["replicas"]:
        rp["sessions"] = per_replica.get(rp["name"], 0)
    return {**snap, "phases": phases, "total_sessions": len(rows)}


class ModeIn(BaseModel):
    mode: str = Field(pattern=r"^(plain|sandbox|routing|anton)$")


@app.put("/api/mode")
def set_mode(body: ModeIn, p: Principal = Depends(owner)):
    kube.set_mode(body.mode)
    audit(p, "mode.set", body.mode)
    return {"mode": body.mode}


# ---------- benchmarks ----------

class BenchIn(BaseModel):
    duration: str = Field(default="3m", pattern=r"^\d{1,3}[sm]$")
    sessions: int = Field(default=24, ge=2, le=60)
    seeds: int = Field(default=1, ge=1, le=5, description="repeat every mode with seeds 1..n")
    spot_deadline: str = Field(default="10s", pattern=r"^(10s|30s|120s)$", description="spot-interruption notice")


SAMPLE_METRICS = ["ttft_p50_ms", "ttft_p95_ms", "resume_ttft_p50_ms", "resume_ttft_p95_ms", "recomputed_tokens",
                  "cached_tokens", "prefill_gpu_seconds", "requests", "errors", "affected_sessions", "lost_kv_state",
                  "wake_ttft_p50_ms", "wake_kv_preserved", "workspace_preserved", "hibernated",
                  "sandboxes_disrupted", "workspace_lost", "workspace_unchecked", "spot_affected", "spot_rescued"]
AVERAGED = {"ttft_p50_ms", "ttft_p95_ms", "resume_ttft_p50_ms", "resume_ttft_p95_ms", "wake_ttft_p50_ms"}


def combine_by_mode(results: list[dict]) -> list[dict]:
    """One entry per mode across seeds: counts summed, latency percentiles
    averaged (same rule as the bench's own combined table). `seeds` says how
    many runs went in; `gpu_s_per_request_by_seed` keeps the spread visible."""
    order, groups = [], {}
    for r in results:
        if r.get("mode") not in groups:
            order.append(r.get("mode"))
            groups[r.get("mode")] = []
        groups[r.get("mode")].append(r)
    out = []
    for mode in order:
        g = groups[mode]
        c = {"mode": mode, "seeds": len(g),
             "gpu_s_per_request_by_seed": [r.get("prefill_gpu_seconds", 0) / max(1, r.get("requests", 0) - r.get("errors", 0)) for r in g]}
        for k in {k for r in g for k, v in r.items() if isinstance(v, (int, float)) and k != "seed"}:
            vals = [r.get(k, 0) for r in g]
            c[k] = sum(vals) / len(vals) if k in AVERAGED else sum(vals)
        out.append(c)
    return out


def store_samples(db, run: BenchmarkRun) -> None:
    """Flattens each mode's result into benchmark_samples (idempotent)."""
    have = {(s.mode, s.metric) for s in db.query(BenchmarkSample).filter(BenchmarkSample.run_id == run.id)}
    for res in run.results:
        mode = res.get("mode", "")
        for metric in SAMPLE_METRICS:
            if (mode, metric) not in have and isinstance(res.get(metric), (int, float)):
                db.add(BenchmarkSample(run_id=run.id, mode=mode, metric=metric, value=float(res[metric])))


def refresh(db, run: BenchmarkRun) -> None:
    if run.status != "running":
        return
    status, results = kube.bench_status(run.job)
    run.results = combine_by_mode(results)
    store_samples(db, run)
    if status != "running":
        run.status, run.finished_at = status, now()


def run_json(r: BenchmarkRun) -> dict:
    return {"id": r.id, "job": r.job, "status": r.status, "params": r.params, "launched_by": r.launched_by,
            "results": r.results, "started_at": r.started_at, "finished_at": r.finished_at}


@app.get("/api/benchmarks")
def benchmarks(p: Principal = Depends(current)):
    with SessionLocal() as db:
        runs = db.query(BenchmarkRun).order_by(BenchmarkRun.id.desc()).limit(20).all()
        for run in runs:
            refresh(db, run)
        db.commit()
        return {"runs": [run_json(r) for r in runs]}


@app.get("/api/benchmarks/samples")
def benchmark_samples(metric: str = Query(pattern=r"^[a-z0-9_]{1,48}$"), p: Principal = Depends(current)):
    """One metric across recent runs, for trend charts."""
    with SessionLocal() as db:
        rows = db.query(BenchmarkSample, BenchmarkRun.job, BenchmarkRun.started_at) \
            .join(BenchmarkRun, BenchmarkRun.id == BenchmarkSample.run_id) \
            .filter(BenchmarkSample.metric == metric).order_by(BenchmarkRun.id).all()
    return {"metric": metric, "samples": [{"job": j, "at": at, "mode": s.mode, "value": s.value} for s, j, at in rows]}


@app.post("/api/benchmarks", status_code=201)
def launch_benchmark(body: BenchIn, p: Principal = Depends(current)):
    with SessionLocal() as db:
        if db.query(BenchmarkRun).filter(BenchmarkRun.status == "running").count():
            raise HTTPException(409, "a benchmark is already running")
        q = quota_of(db, p.team_id)
        since = now() - timedelta(days=1)
        if db.query(BenchmarkRun).filter(BenchmarkRun.team_id == p.team_id, BenchmarkRun.started_at >= since).count() \
                >= q.max_benchmarks_per_day:
            raise HTTPException(429, f"team limit of {q.max_benchmarks_per_day} benchmark runs per day reached")
        job = "anton-bench-" + re.sub(r"[^0-9]", "", now().isoformat())[:14]
        kube.launch_bench(job, body.duration, body.sessions, ",".join(str(i) for i in range(1, body.seeds + 1)),
                          body.spot_deadline)
        db.add(BenchmarkRun(job=job, team_id=p.team_id, launched_by=p.user, params=body.model_dump()))
        db.commit()
    audit(p, "benchmark.launch", job, **body.model_dump())
    return {"job": job}


# ---------- live events ----------

@app.websocket("/ws")
async def events(ws: WebSocket, token: str = ""):
    p = auth.principal_from_token(token)
    team = p.team if p else None
    if team is None:
        await ws.close(code=4401)
        return
    await ws.accept()
    with SessionLocal() as db:
        last = db.query(LifecycleEvent.id).order_by(LifecycleEvent.id.desc()).limit(1).scalar() or 0
    try:
        while True:
            await asyncio.sleep(1)
            with SessionLocal() as db:
                # Filter on the recorded team, never on the session-name prefix:
                # team "dev" must not see team "dev-ops".
                new = db.query(LifecycleEvent).filter(LifecycleEvent.id > last, LifecycleEvent.team == team) \
                    .order_by(LifecycleEvent.id).all()
            for e in new:
                last = e.id
                await ws.send_json({"id": e.id, "session": e.session, "kind": e.kind,
                                    "detail": e.detail, "at": e.at.isoformat()})
    except (WebSocketDisconnect, RuntimeError):
        pass


@app.get("/healthz")
def healthz():
    return {"ok": True}


# The built web UI (static export) is served from the same origin.
static = Path(os.environ.get("STATIC_DIR", "/app/static"))
if static.is_dir():
    app.mount("/", StaticFiles(directory=static, html=True), name="web")
