"""Alembic environment: migrations run against DATABASE_URL using the models
in app.db as the target for autogenerate."""
import os

from alembic import context
from sqlalchemy import engine_from_config, pool

from app.db import Base

config = context.config
if not config.get_main_option("sqlalchemy.url"):
    config.set_main_option("sqlalchemy.url", os.environ.get("DATABASE_URL", "sqlite:///./anton-dev.db").replace("%", "%%"))


def run() -> None:
    if context.is_offline_mode():
        context.configure(url=config.get_main_option("sqlalchemy.url"), target_metadata=Base.metadata,
                          literal_binds=True, render_as_batch=True)
        with context.begin_transaction():
            context.run_migrations()
        return
    connectable = engine_from_config(config.get_section(config.config_ini_section, {}),
                                     prefix="sqlalchemy.", poolclass=pool.NullPool)
    with connectable.connect() as conn:
        # render_as_batch lets the same migrations run on SQLite (tests).
        context.configure(connection=conn, target_metadata=Base.metadata, render_as_batch=True)
        with context.begin_transaction():
            context.run_migrations()


run()
