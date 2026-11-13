"use client";

import { useCallback, useEffect, useState } from "react";
import { Auth, Template, api, idleLabel } from "./lib";

type TeamData = {
  name: string; role: string;
  members: { id: number; name: string; email: string | null; role: string }[];
  quota: { max_sessions: number; max_active_sessions: number; max_benchmarks_per_day: number; requests_per_minute: number };
  usage: { sessions: number; active_sessions: number; benchmarks_today: number };
};
type Key = { id: number; name: string; prefix: string; created_at: string; last_used_at: string | null };
type Audit = { id: number; at: string; actor: string; action: string; target: string; detail: Record<string, unknown> };

const Meter = ({ label, used, max }: { label: string; used: number; max: number }) => (
  <div className="card">
    <div className="muted small">{label}</div>
    <div className="stat">{used} <span className="muted small">/ {max}</span></div>
    <div className="meter"><div style={{ width: `${Math.min(100, (100 * used) / Math.max(1, max))}%` }} className={used >= max ? "full" : ""} /></div>
  </div>
);

export function Team({ auth, tick }: { auth: Auth; tick: number }) {
  const [t, setT] = useState<TeamData | null>(null);
  const [keys, setKeys] = useState<Key[]>([]);
  const [templates, setTemplates] = useState<Template[]>([]);
  const [audit, setAudit] = useState<Audit[]>([]);
  const [invite, setInvite] = useState("");
  const [keyName, setKeyName] = useState("");
  const [newKey, setNewKey] = useState("");
  const [tpl, setTpl] = useState({ name: "", description: "", priority: 0, idle_timeout_seconds: 0 });
  const [err, setErr] = useState("");

  const load = useCallback(() => {
    api<TeamData>(auth, "/api/team").then((d) => {
      setT(d);
      if (d.role === "owner") api<{ entries: Audit[] }>(auth, "/api/audit?limit=50").then((r) => setAudit(r.entries)).catch(() => {});
    }).catch((e) => setErr(e.message));
    api<{ keys: Key[] }>(auth, "/api/team/keys").then((r) => setKeys(r.keys)).catch(() => {});
    api<{ templates: Template[] }>(auth, "/api/templates").then((r) => setTemplates(r.templates)).catch(() => {});
  }, [auth]);
  useEffect(load, [load, tick]);
  const act = (p: Promise<unknown>) => p.then(() => { setErr(""); load(); }).catch((e) => setErr(e.message));

  if (!t) return <p className="muted">{err || "Loading…"}</p>;
  const owner = t.role === "owner";
  return (
    <>
      <div className="panel">
        <h2>Team {t.name} <span className="muted small">· you are {t.role === "owner" ? "an owner" : "a member"}</span></h2>
        <div className="cards">
          <Meter label="Active sessions" used={t.usage.active_sessions} max={t.quota.max_active_sessions} />
          <Meter label="All sessions (incl. hibernated)" used={t.usage.sessions} max={t.quota.max_sessions} />
          <Meter label="Benchmark runs today" used={t.usage.benchmarks_today} max={t.quota.max_benchmarks_per_day} />
          <div className="card"><div className="muted small">API rate limit</div><div className="stat">{t.quota.requests_per_minute}<span className="muted small"> / min per user</span></div></div>
        </div>
        <p className="muted small">Hibernated sessions do not count toward the active limit: hibernating is how a team stays under it.</p>
        {err && <p className="err small">{err}</p>}
      </div>

      <div className="panel">
        <h2>Members</h2>
        <table>
          <tbody>
            {t.members.map((m) => (
              <tr key={m.id}>
                <td>{m.name}</td><td className="muted">{m.email || ""}</td><td>{m.role}</td>
                <td>{owner && m.name !== auth.name && <button onClick={() => act(api(auth, `/api/team/members/${m.id}`, "DELETE"))}>Remove</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {owner && (
          <form className="row" style={{ marginTop: 12 }} onSubmit={(e) => { e.preventDefault(); act(api(auth, "/api/team/members", "POST", { user: invite }).then(() => setInvite(""))); }}>
            <input className="grow" aria-label="invite" placeholder="user name, or email for Google sign-in" value={invite} onChange={(e) => setInvite(e.target.value)} />
            <button className="primary" disabled={!invite}>Invite</button>
          </form>
        )}
      </div>

      <div className="panel">
        <h2>Session templates</h2>
        <table>
          <tbody>
            {templates.map((x) => (
              <tr key={x.id}>
                <td>{x.name}{x.builtin && <span className="muted small"> · built-in</span>}</td>
                <td className="muted">priority {x.priority} · {idleLabel(x.idle_timeout_seconds)}</td>
                <td className="muted small">{x.description}</td>
                <td>{owner && !x.builtin && <button onClick={() => act(api(auth, `/api/templates/${x.id}`, "DELETE"))}>Delete</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {owner && (
          <form className="row" style={{ marginTop: 12 }} onSubmit={(e) => { e.preventDefault(); act(api(auth, "/api/templates", "POST", tpl).then(() => setTpl({ ...tpl, name: "", description: "" }))); }}>
            <input aria-label="template name" placeholder="name" value={tpl.name} onChange={(e) => setTpl({ ...tpl, name: e.target.value })} style={{ width: 120 }} />
            <input className="grow" aria-label="description" placeholder="description" value={tpl.description} onChange={(e) => setTpl({ ...tpl, description: e.target.value })} />
            <label className="small muted">Priority <input type="number" min={0} max={10} value={tpl.priority} onChange={(e) => setTpl({ ...tpl, priority: +e.target.value })} style={{ width: 60 }} /></label>
            <label className="small muted">Idle s <input type="number" min={0} max={86400} value={tpl.idle_timeout_seconds} onChange={(e) => setTpl({ ...tpl, idle_timeout_seconds: +e.target.value })} style={{ width: 80 }} /></label>
            <button className="primary" disabled={!tpl.name}>Add</button>
          </form>
        )}
      </div>

      <div className="panel">
        <h2>Your API keys</h2>
        <p className="muted small">For scripts and CI: send <code>Authorization: Bearer &lt;key&gt;</code>. Keys act as you, in this team.</p>
        {keys.length > 0 && (
          <table>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id}>
                  <td>{k.name}</td><td className="muted"><code>{k.prefix}…</code></td>
                  <td className="muted small">last used {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "never"}</td>
                  <td><button onClick={() => act(api(auth, `/api/team/keys/${k.id}`, "DELETE"))}>Revoke</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <form className="row" style={{ marginTop: 12 }} onSubmit={(e) => { e.preventDefault(); api<{ key: string }>(auth, "/api/team/keys", "POST", { name: keyName }).then((r) => { setNewKey(r.key); setKeyName(""); load(); }).catch((x) => setErr(x.message)); }}>
          <input className="grow" aria-label="key name" placeholder="key name, e.g. ci" value={keyName} onChange={(e) => setKeyName(e.target.value)} />
          <button className="primary" disabled={!keyName}>Create key</button>
        </form>
        {newKey && <p className="banner good small">New key (shown once, copy it now): <code>{newKey}</code></p>}
      </div>

      {owner && (
        <div className="panel">
          <h2>Audit log</h2>
          <div className="tablewrap">
            <table>
              <thead><tr><th>When</th><th>Who</th><th>Action</th><th>Target</th><th>Detail</th></tr></thead>
              <tbody>
                {audit.map((a) => (
                  <tr key={a.id}>
                    <td className="muted small">{new Date(a.at).toLocaleString()}</td><td>{a.actor}</td><td>{a.action}</td>
                    <td className="muted">{a.target}</td><td className="muted small">{Object.keys(a.detail || {}).length ? JSON.stringify(a.detail) : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </>
  );
}
