"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, Auth, Badge, LiveEvent, Msg, Session, Template, api, idleLabel, ms, time } from "./lib";
import Terminal from "./terminal";

// ---------- sessions list ----------

type ListData = {
  sessions: Session[]; max_sessions: number; max_active_sessions: number;
  usage: { sessions: number; active_sessions: number };
};

export function Sessions({ auth, tick, onOpen, onAuthLost }: { auth: Auth; tick: number; onOpen: (n: string) => void; onAuthLost: () => void }) {
  const [d, setD] = useState<ListData | null>(null);
  const [templates, setTemplates] = useState<Template[]>([]);
  const [label, setLabel] = useState("agent");
  const [template, setTemplate] = useState("agent");
  const [err, setErr] = useState("");

  const fail = useCallback((e: Error) => { if (e instanceof ApiError && e.status === 401) onAuthLost(); else setErr(e.message); }, [onAuthLost]);
  const load = useCallback(() => { api<ListData>(auth, "/api/sessions").then(setD).catch(fail); }, [auth, fail]);
  useEffect(load, [load, tick]);
  useEffect(() => { api<{ templates: Template[] }>(auth, "/api/templates").then((r) => setTemplates(r.templates)).catch(fail); }, [auth, fail]);

  const act = (p: Promise<unknown>) => p.then(() => { setErr(""); load(); }).catch(fail);
  const chosen = templates.find((t) => t.name === template);
  return (
    <>
      <div className="panel">
        <h2>New agent session</h2>
        <div className="row">
          <input aria-label="label" placeholder="label" value={label} onChange={(e) => setLabel(e.target.value)} style={{ width: 140 }} />
          <label className="small muted">Template{" "}
            <select value={template} onChange={(e) => setTemplate(e.target.value)}>
              {templates.map((t) => <option key={t.id} value={t.name}>{t.name}{t.builtin ? "" : " (team)"}</option>)}
            </select>
          </label>
          <button className="primary" onClick={() => act(api(auth, "/api/sessions", "POST", { label, template }))}>Launch</button>
          {d && (
            <span className="spacer muted small">
              {d.usage.active_sessions} of {d.max_active_sessions} active · {d.usage.sessions} of {d.max_sessions} sessions
            </span>
          )}
        </div>
        {chosen && (
          <p className="muted small" style={{ margin: "8px 0 0" }}>
            {chosen.description || "No description."} Priority {chosen.priority}, {idleLabel(chosen.idle_timeout_seconds)}.
          </p>
        )}
        {err && <p className="err small">{err}</p>}
      </div>
      <div className="panel">
        <h2>Sessions</h2>
        {!d ? <p className="muted">Loading…</p> : d.sessions.length === 0 ? <p className="muted">No sessions yet. Launch one above.</p> : (
          <div className="tablewrap">
            <table>
              <thead>
                <tr><th>Name</th><th>Phase</th><th>Owner</th><th>Node</th><th>KV cache</th><th className="num">Context</th><th className="num">Rescues</th><th></th></tr>
              </thead>
              <tbody>
                {d.sessions.map((s) => (
                  <tr key={s.name} className="click" onClick={() => onOpen(s.name)}>
                    <td>{s.name}{s.priority > 0 && <span className="muted small"> · priority {s.priority}</span>}</td>
                    <td><Badge v={s.phase} /></td>
                    <td className="muted">{s.owner || "-"}</td>
                    <td className="muted">{s.node || "-"}</td>
                    <td className="muted">{s.cache_location === "tier" ? "in cache tier" : s.replica || "-"}</td>
                    <td className="num">{s.context_tokens ? `${s.context_tokens.toLocaleString()} tok` : "-"}</td>
                    <td className="num">{s.rescues}</td>
                    <td onClick={(e) => e.stopPropagation()}>
                      <div className="row">
                        {s.desired === "Hibernated"
                          ? <button onClick={() => act(api(auth, `/api/sessions/${s.name}/resume`, "POST"))}>Resume</button>
                          : <button onClick={() => act(api(auth, `/api/sessions/${s.name}/hibernate`, "POST"))}>Hibernate</button>}
                        <button onClick={() => { if (confirm(`Delete ${s.name}?`)) act(api(auth, `/api/sessions/${s.name}`, "DELETE")); }}>Delete</button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  );
}

// ---------- session workspace ----------

type DetailData = {
  session: Session; messages: Msg[]; events: LiveEvent[];
  workspace: { workspace: string; lines: number; restored: boolean; source?: string } | null;
};

const DISRUPTIONS = ["Moved", "Migrating", "Rescuing", "Waking", "KVLost", "KVOffloaded"];

// The most recent disruption, and how the first reply after it went: the
// "was I moved, and how fast did I come back" indicator.
function lastDisruption(d: DetailData) {
  const ev = d.events.find((e) => DISRUPTIONS.includes(e.kind)); // events are newest first
  if (!ev) return null;
  const after = d.messages.find((m) => m.role === "assistant" && m.ttft_ms != null && new Date(m.at) >= new Date(ev.at));
  return { ev, after };
}

export function Detail({ auth, name, tick, onBack }: { auth: Auth; name: string; tick: number; onBack: () => void }) {
  const [d, setD] = useState<DetailData | null>(null);
  const [view, setView] = useState<"chat" | "terminal" | "files">("chat");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const end = useRef<HTMLDivElement>(null);

  const load = useCallback(() => { api<DetailData>(auth, `/api/sessions/${name}`).then(setD).catch((e) => setErr(e.message)); }, [auth, name]);
  useEffect(load, [load, tick]);
  useEffect(() => { end.current?.scrollIntoView({ block: "nearest" }); }, [d?.messages.length, view]);

  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true); setErr("");
    try {
      await api(auth, `/api/sessions/${name}/messages`, "POST", { content: text });
      setText("");
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false); load();
  };

  if (!d) return <p className="muted">{err || "Loading…"}</p>;
  const s = d.session;
  const running = s.phase === "Active" && !!d.workspace;
  const disruption = lastDisruption(d);
  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <button onClick={onBack}>← Sessions</button>
        <strong>{s.name}</strong> <Badge v={s.phase} />
        <span className="muted small">{s.message}</span>
      </div>
      {disruption && (
        <div className={`banner ${disruption.after && (disruption.after.cached_tokens ?? 0) > 0 ? "good" : "warn"}`}>
          <strong>{disruption.ev.kind}</strong> at {time(disruption.ev.at)}: {disruption.ev.detail || "the session was disrupted"}.{" "}
          {disruption.after
            ? <>The next reply came back in <strong>{ms(disruption.after.ttft_ms!)}</strong> with {(disruption.after.cached_tokens ?? 0).toLocaleString()} tokens still cached and {(disruption.after.recomputed_tokens ?? 0).toLocaleString()} recomputed{disruption.after.replica ? ` on ${disruption.after.replica}` : ""}.</>
            : <>No reply since; the next message shows how fast it comes back.</>}
        </div>
      )}
      <div className="cols">
        <div className="panel">
          <div className="row" style={{ marginBottom: 12 }}>
            {(["chat", "terminal", "files"] as const).map((v) => (
              <button key={v} className={`tab ${view === v ? "active" : ""}`} onClick={() => setView(v)}>
                {{ chat: "Agent", terminal: "Terminal", files: "Files" }[v]}
              </button>
            ))}
          </div>
          {view === "chat" && (
            <>
              <div className="chat">
                {d.messages.length === 0 && <p className="muted">Send a message. A hibernated session wakes on the first one.</p>}
                {d.messages.map((m) => (
                  <div key={m.id} className={`msg ${m.role}`}>
                    {m.content}
                    {m.ttft_ms != null && (
                      <div className="muted small">
                        first token in {ms(m.ttft_ms)} · {(m.cached_tokens ?? 0).toLocaleString()} cached · {(m.recomputed_tokens ?? 0).toLocaleString()} recomputed
                      </div>
                    )}
                  </div>
                ))}
                <div ref={end} />
              </div>
              <form className="row" style={{ marginTop: 12 }} onSubmit={send}>
                <input className="grow" aria-label="message" placeholder="Message the agent" value={text} onChange={(e) => setText(e.target.value)} />
                <button className="primary" disabled={busy || !text}>{busy ? "Waiting…" : "Send"}</button>
              </form>
              {err && <p className="err small">{err}</p>}
            </>
          )}
          {view === "terminal" && <Terminal auth={auth} session={name} running={running} />}
          {view === "files" && <Files auth={auth} session={name} running={running} tick={tick} />}
        </div>
        <div>
          <div className="panel">
            <h2>State</h2>
            <table>
              <tbody>
                <tr><th>Owner</th><td>{s.owner || "-"}</td></tr>
                <tr><th>Sandbox node</th><td>{s.node || "-"}</td></tr>
                <tr><th>Inference replica</th><td>{s.replica || "-"}</td></tr>
                <tr><th>KV cache</th><td>{s.cache_location || "-"} · {s.cached_tokens.toLocaleString()} tok</td></tr>
                <tr><th>Workspace</th><td>{d.workspace ? `${d.workspace.lines} journal lines${d.workspace.restored ? ` · restored from ${d.workspace.source || "snapshot"}` : ""}` : "sandbox not running"}</td></tr>
                <tr><th>Hibernations / rescues</th><td>{s.hibernations} / {s.rescues}</td></tr>
              </tbody>
            </table>
          </div>
          <div className="panel">
            <h2>Timeline</h2>
            <ul className="timeline">
              {d.events.map((e) => (
                <li key={e.id}>
                  <Badge v={e.kind} /> <span className="muted small">{time(e.at)}</span>
                  <div className="small muted">{e.detail}</div>
                </li>
              ))}
            </ul>
          </div>
        </div>
      </div>
    </>
  );
}

// ---------- file tree ----------

type FileEntry = { path: string; size: number; dir: boolean };

function Files({ auth, session, running, tick }: { auth: Auth; session: string; running: boolean; tick: number }) {
  const [files, setFiles] = useState<FileEntry[] | null>(null);
  const [open, setOpen] = useState<{ path: string; content: string; truncated: boolean } | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    if (!running) return;
    api<{ files: FileEntry[] }>(auth, `/api/sessions/${session}/files`).then((r) => { setFiles(r.files); setErr(""); }).catch((e) => setErr(e.message));
  }, [auth, session, running, tick]);
  const show = (path: string) =>
    api<{ path: string; content: string; truncated: boolean }>(auth, `/api/sessions/${session}/files/content?path=${encodeURIComponent(path)}`)
      .then(setOpen).catch((e) => setErr(e.message));

  if (!running) return <p className="muted">The sandbox is not running, so its files are in the snapshot. Wake the session to browse them.</p>;
  if (!files) return <p className="muted">{err || "Loading…"}</p>;
  const sorted = [...files].sort((a, b) => a.path.localeCompare(b.path));
  return (
    <div className="files">
      <ul className="tree">
        {sorted.map((f) => {
          const depth = f.path.split("/").length - 1;
          const base = f.path.split("/").pop();
          return (
            <li key={f.path} style={{ paddingLeft: depth * 14 }}>
              {f.dir ? <span className="muted">{base}/</span> : (
                <button className={`link ${open?.path === f.path ? "active" : ""}`} onClick={() => show(f.path)}>
                  {base} <span className="muted small">{f.size} B</span>
                </button>
              )}
            </li>
          );
        })}
      </ul>
      <div className="viewer">
        {open ? (
          <>
            <div className="small muted">{open.path}{open.truncated ? " (first 64 KB)" : ""}</div>
            <pre>{open.content}</pre>
          </>
        ) : <p className="muted">Pick a file.</p>}
        {err && <p className="err small">{err}</p>}
      </div>
    </div>
  );
}
