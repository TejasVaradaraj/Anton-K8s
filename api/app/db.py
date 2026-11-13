"""Database models. Postgres owns history, identity, quotas and the audit log;
the cluster owns live state. The `sessions` table is a mirror the sync worker
can rebuild from the cluster; everything else is Postgres-only.

The schema is managed by Alembic (api/migrations). `migrate()` upgrades to
head and is what the API, the tests and the init container call."""
import os
from datetime import datetime, timezone
from pathlib import Path

from sqlalchemy import (JSON, Boolean, DateTime, Float, ForeignKey, Integer, String, Text, UniqueConstraint,
                        create_engine)
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, sessionmaker

DATABASE_URL = os.environ.get("DATABASE_URL", "sqlite:///./anton-dev.db")
engine = create_engine(DATABASE_URL, pool_pre_ping=True)
SessionLocal = sessionmaker(engine, expire_on_commit=False)


def now() -> datetime:
    return datetime.now(timezone.utc)


class Base(DeclarativeBase):
    pass


class Team(Base):
    __tablename__ = "teams"
    id: Mapped[int] = mapped_column(primary_key=True)
    name: Mapped[str] = mapped_column(String(63), unique=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class User(Base):
    __tablename__ = "users"
    id: Mapped[int] = mapped_column(primary_key=True)
    name: Mapped[str] = mapped_column(String(63), unique=True)
    email: Mapped[str | None] = mapped_column(String(254), unique=True, default=None)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class Membership(Base):
    """A user can belong to several teams; role is owner or member. Owners
    manage members, templates and quotas and can read the audit log."""
    __tablename__ = "memberships"
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), primary_key=True)
    team_id: Mapped[int] = mapped_column(ForeignKey("teams.id", ondelete="CASCADE"), primary_key=True)
    role: Mapped[str] = mapped_column(String(16), default="member")
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class ApiKey(Base):
    """A bearer credential scoped to one user acting in one team. Login issues
    one (kind=login); users can mint long-lived ones for scripts (kind=api).
    Only a SHA-256 hash is stored; the key is shown once."""
    __tablename__ = "api_keys"
    id: Mapped[int] = mapped_column(primary_key=True)
    user_id: Mapped[int] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"), index=True)
    team_id: Mapped[int] = mapped_column(ForeignKey("teams.id", ondelete="CASCADE"))
    kind: Mapped[str] = mapped_column(String(8), default="login")
    name: Mapped[str] = mapped_column(String(63), default="")
    prefix: Mapped[str] = mapped_column(String(12))
    hash: Mapped[str] = mapped_column(String(64), unique=True, index=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    last_used_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    revoked: Mapped[bool] = mapped_column(Boolean, default=False)


class Quota(Base):
    """Per-team limits. Hibernated sessions count toward max_sessions but not
    toward max_active_sessions: hibernation is how a team stays under quota."""
    __tablename__ = "quotas"
    team_id: Mapped[int] = mapped_column(ForeignKey("teams.id", ondelete="CASCADE"), primary_key=True)
    max_sessions: Mapped[int] = mapped_column(Integer, default=10)
    max_active_sessions: Mapped[int] = mapped_column(Integer, default=6)
    max_benchmarks_per_day: Mapped[int] = mapped_column(Integer, default=10)
    requests_per_minute: Mapped[int] = mapped_column(Integer, default=600)


class Template(Base):
    """A session preset. team_id NULL means a built-in template every team sees."""
    __tablename__ = "templates"
    __table_args__ = (UniqueConstraint("team_id", "name"),)
    id: Mapped[int] = mapped_column(primary_key=True)
    team_id: Mapped[int | None] = mapped_column(ForeignKey("teams.id", ondelete="CASCADE"), default=None)
    name: Mapped[str] = mapped_column(String(31))
    description: Mapped[str] = mapped_column(Text, default="")
    model: Mapped[str] = mapped_column(String(63), default="mock")
    priority: Mapped[int] = mapped_column(Integer, default=0)
    idle_timeout_seconds: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class SessionRow(Base):
    """A mirror of one AgentSession resource, written by the sync worker
    (plus API-owned columns: owner, template_id, context_tokens)."""
    __tablename__ = "sessions"
    name: Mapped[str] = mapped_column(String(63), primary_key=True)
    team: Mapped[str] = mapped_column(String(63), index=True, default="")
    owner: Mapped[str] = mapped_column(String(63), default="")
    template_id: Mapped[int | None] = mapped_column(ForeignKey("templates.id", ondelete="SET NULL"), default=None)
    model: Mapped[str] = mapped_column(String(63), default="")
    priority: Mapped[int] = mapped_column(Integer, default=0)
    desired: Mapped[str] = mapped_column(String(16), default="Active")
    phase: Mapped[str] = mapped_column(String(16), default="Pending")
    node: Mapped[str] = mapped_column(String(63), default="")
    replica: Mapped[str] = mapped_column(String(63), default="")
    cache_location: Mapped[str] = mapped_column(String(16), default="")
    cached_tokens: Mapped[int] = mapped_column(Integer, default=0)
    message: Mapped[str] = mapped_column(Text, default="")
    hibernations: Mapped[int] = mapped_column(Integer, default=0)
    rescues: Mapped[int] = mapped_column(Integer, default=0)
    context_tokens: Mapped[int] = mapped_column(Integer, default=0)
    resource_version: Mapped[str] = mapped_column(String(32), default="")
    deleted: Mapped[bool] = mapped_column(Boolean, default=False)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    updated_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class Message(Base):
    __tablename__ = "messages"
    id: Mapped[int] = mapped_column(primary_key=True)
    session: Mapped[str] = mapped_column(String(63), index=True)
    role: Mapped[str] = mapped_column(String(16))
    content: Mapped[str] = mapped_column(Text)
    ttft_ms: Mapped[float | None] = mapped_column(default=None)
    cached_tokens: Mapped[int | None] = mapped_column(default=None)
    recomputed_tokens: Mapped[int | None] = mapped_column(default=None)
    replica: Mapped[str | None] = mapped_column(String(63), default=None)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class LifecycleEvent(Base):
    """What happened to a session, as observed in the cluster by the sync
    worker: one row per phase, replica or cache-location change. Append-only."""
    __tablename__ = "lifecycle_events"
    id: Mapped[int] = mapped_column(primary_key=True)
    session: Mapped[str] = mapped_column(String(63), index=True)
    team: Mapped[str] = mapped_column(String(63), index=True, default="")
    kind: Mapped[str] = mapped_column(String(32))
    detail: Mapped[str] = mapped_column(Text, default="")
    at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)


class AuditEntry(Base):
    """Who asked for what, as received by the API: every state-changing call.
    Append-only. Together with the cluster it lets Postgres be rebuilt."""
    __tablename__ = "audit_log"
    id: Mapped[int] = mapped_column(primary_key=True)
    at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now, index=True)
    team_id: Mapped[int | None] = mapped_column(ForeignKey("teams.id", ondelete="SET NULL"), index=True, default=None)
    user_id: Mapped[int | None] = mapped_column(ForeignKey("users.id", ondelete="SET NULL"), default=None)
    actor: Mapped[str] = mapped_column(String(63), default="")
    action: Mapped[str] = mapped_column(String(48))
    target: Mapped[str] = mapped_column(String(127), default="")
    detail: Mapped[dict] = mapped_column(JSON, default=dict)


class BenchmarkRun(Base):
    __tablename__ = "benchmark_runs"
    id: Mapped[int] = mapped_column(primary_key=True)
    job: Mapped[str] = mapped_column(String(63), unique=True)
    team_id: Mapped[int | None] = mapped_column(ForeignKey("teams.id", ondelete="SET NULL"), default=None)
    launched_by: Mapped[str] = mapped_column(String(63), default="")
    status: Mapped[str] = mapped_column(String(16), default="running")
    params: Mapped[dict] = mapped_column(JSON, default=dict)
    results: Mapped[list] = mapped_column(JSON, default=list)
    started_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    finished_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)


class BenchmarkSample(Base):
    """One number from one mode of one run, flattened from the run's results
    so runs can be compared and charted with plain SQL."""
    __tablename__ = "benchmark_samples"
    __table_args__ = (UniqueConstraint("run_id", "mode", "metric"),)
    id: Mapped[int] = mapped_column(primary_key=True)
    run_id: Mapped[int] = mapped_column(ForeignKey("benchmark_runs.id", ondelete="CASCADE"), index=True)
    mode: Mapped[str] = mapped_column(String(16))
    metric: Mapped[str] = mapped_column(String(48))
    value: Mapped[float] = mapped_column(Float)


BUILTIN_TEMPLATES = [
    {"name": "agent", "description": "Interactive coding agent; never hibernates on its own.", "idle_timeout_seconds": 0},
    {"name": "background", "description": "Long-running task; hibernates after 10 idle minutes.", "idle_timeout_seconds": 600},
    {"name": "priority", "description": "Rescued first when a node is about to disappear.", "priority": 10},
]


def migrate() -> None:
    """Upgrade the schema to the newest Alembic revision and seed built-ins."""
    from alembic import command
    from alembic.config import Config

    here = Path(__file__).resolve().parent.parent
    cfg = Config(str(here / "alembic.ini"))
    cfg.set_main_option("script_location", str(here / "migrations"))
    cfg.set_main_option("sqlalchemy.url", DATABASE_URL.replace("%", "%%"))
    command.upgrade(cfg, "head")
    with SessionLocal() as db:
        for t in BUILTIN_TEMPLATES:
            if not db.query(Template).filter(Template.team_id.is_(None), Template.name == t["name"]).first():
                db.add(Template(**t))
        db.commit()
