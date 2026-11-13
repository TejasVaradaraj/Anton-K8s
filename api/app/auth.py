"""Identity: who is calling and on behalf of which team.

Every request carries `Authorization: Bearer <key>`. A key belongs to one
user acting in one team; only its SHA-256 hash is stored. Keys come from:

- Google OAuth (when GOOGLE_CLIENT_ID/SECRET are set): /api/auth/google/start
  redirects to Google; the callback exchanges the code server-side, reads the
  verified email, and issues a login key for the team chosen at start.
- Dev login (when DEV_LOGIN=1, the default without Google): name + team.
- POST /api/team/keys: long-lived keys for scripts and CI.

Team membership rules: the first person into a new team becomes its owner.
Joining an existing team needs an invitation from an owner (by email or user
name), except in dev login, where anyone may join as a member."""
import hashlib
import os
import secrets
import time
from dataclasses import dataclass
from urllib.parse import urlencode

import httpx
from fastapi import Depends, Header, HTTPException, Request

from .db import ApiKey, Membership, Quota, SessionLocal, Team, User, now
from .ratelimit import limiter

GOOGLE_CLIENT_ID = os.environ.get("GOOGLE_CLIENT_ID", "")
GOOGLE_CLIENT_SECRET = os.environ.get("GOOGLE_CLIENT_SECRET", "")
PUBLIC_URL = os.environ.get("PUBLIC_URL", "http://localhost:8000")
GOOGLE_ENABLED = bool(GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET)
DEV_LOGIN = os.environ.get("DEV_LOGIN", "0" if GOOGLE_ENABLED else "1") == "1"


@dataclass
class Principal:
    user_id: int
    user: str
    team_id: int
    team: str
    role: str
    key_id: int

    @property
    def owner(self) -> bool:
        return self.role == "owner"


def hash_key(key: str) -> str:
    return hashlib.sha256(key.encode()).hexdigest()


def issue_key(db, user: User, team: Team, kind: str = "login", name: str = "") -> str:
    key = "ant_" + secrets.token_urlsafe(32)
    db.add(ApiKey(user_id=user.id, team_id=team.id, kind=kind, name=name, prefix=key[:12], hash=hash_key(key)))
    return key


def join_team(db, user: User, team_name: str, allow_open_join: bool) -> tuple[Team, Membership]:
    """Puts the user in the team, creating it (with a default quota) if new."""
    team = db.query(Team).filter(Team.name == team_name).first()
    if team is None:
        team = Team(name=team_name)
        db.add(team)
        db.flush()
        db.add(Quota(team_id=team.id))
        m = Membership(user_id=user.id, team_id=team.id, role="owner")
        db.add(m)
        return team, m
    m = db.get(Membership, (user.id, team.id))
    if m is None:
        if not allow_open_join:
            raise HTTPException(403, "you are not a member of that team; ask an owner to invite you")
        m = Membership(user_id=user.id, team_id=team.id, role="member")
        db.add(m)
    return team, m


def principal_from_token(token: str) -> Principal | None:
    """For WebSockets, which carry the key as a query parameter."""
    if not token:
        return None
    with SessionLocal() as db:
        key = db.query(ApiKey).filter(ApiKey.hash == hash_key(token), ApiKey.revoked.is_(False)).first()
        m = db.get(Membership, (key.user_id, key.team_id)) if key else None
        if m is None:
            return None
        return Principal(key.user_id, db.get(User, key.user_id).name, key.team_id, db.get(Team, key.team_id).name,
                         m.role, key.id)


def current(request: Request, authorization: str = Header(default="")) -> Principal:
    token = authorization.removeprefix("Bearer ").strip()
    if not token:
        raise HTTPException(401, "not logged in")
    with SessionLocal() as db:
        key = db.query(ApiKey).filter(ApiKey.hash == hash_key(token), ApiKey.revoked.is_(False)).first()
        if key is None:
            raise HTTPException(401, "invalid or revoked key")
        m = db.get(Membership, (key.user_id, key.team_id))
        if m is None:
            raise HTTPException(401, "no longer a member of this team")
        user, team = db.get(User, key.user_id), db.get(Team, key.team_id)
        quota = db.get(Quota, team.id)
        if key.last_used_at is None or (now() - _aware(key.last_used_at)).total_seconds() > 60:
            key.last_used_at = now()
            db.commit()
        p = Principal(user.id, user.name, team.id, team.name, m.role, key.id)
    allowed, retry = limiter.hit(f"user:{p.user_id}", quota.requests_per_minute if quota else 600)
    if not allowed:
        raise HTTPException(429, "rate limit exceeded", headers={"Retry-After": str(retry)})
    return p


def owner(p: Principal = Depends(current)) -> Principal:
    if not p.owner:
        raise HTTPException(403, "team owners only")
    return p


def _aware(t):
    from datetime import timezone
    return t if t.tzinfo else t.replace(tzinfo=timezone.utc)


# ---------- Google OAuth (authorization-code flow) ----------
# State is a random value remembered here for 10 minutes along with the team
# the user asked for. One replica is enough for the login flow; with several,
# put the state in Redis or a signed cookie.

_states: dict[str, tuple[float, str]] = {}


def google_start_url(team: str) -> str:
    for s in [s for s, (exp, _) in _states.items() if exp < time.time()]:
        del _states[s]
    state = secrets.token_urlsafe(24)
    _states[state] = (time.time() + 600, team)
    return "https://accounts.google.com/o/oauth2/v2/auth?" + urlencode({
        "client_id": GOOGLE_CLIENT_ID, "redirect_uri": PUBLIC_URL + "/api/auth/google/callback",
        "response_type": "code", "scope": "openid email profile", "state": state, "prompt": "select_account",
    })


def google_finish(code: str, state: str) -> tuple[str, str, str]:
    """Returns (key, user name, team name) after a successful Google login."""
    entry = _states.pop(state, None)
    if entry is None or entry[0] < time.time():
        raise HTTPException(400, "login expired; start again")
    team_name = entry[1]
    tok = httpx.post("https://oauth2.googleapis.com/token", timeout=10, data={
        "code": code, "client_id": GOOGLE_CLIENT_ID, "client_secret": GOOGLE_CLIENT_SECRET,
        "redirect_uri": PUBLIC_URL + "/api/auth/google/callback", "grant_type": "authorization_code",
    })
    if tok.status_code != 200:
        raise HTTPException(401, "Google rejected the login")
    # The access token came straight from Google over TLS, so the userinfo
    # answer can be trusted without verifying the ID token's signature here.
    info = httpx.get("https://openidconnect.googleapis.com/v1/userinfo", timeout=10,
                     headers={"Authorization": f"Bearer {tok.json()['access_token']}"}).json()
    if not info.get("email_verified"):
        raise HTTPException(403, "Google account email is not verified")
    email = info["email"].lower()
    with SessionLocal() as db:
        user = db.query(User).filter(User.email == email).first()
        if user is None:
            base = "".join(c if c.isalnum() else "-" for c in email.split("@")[0].lower())[:24].strip("-") or "user"
            name, n = base, 1
            while db.query(User).filter(User.name == name).first():
                n += 1
                name = f"{base}-{n}"
            user = User(name=name, email=email)
            db.add(user)
            db.flush()
        team, _ = join_team(db, user, team_name, allow_open_join=False)
        key = issue_key(db, user, team)
        db.commit()
        return key, user.name, team.name
