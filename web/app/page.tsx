"use client";

import { useCallback, useEffect, useState } from "react";
import { Bench } from "./bench";
import { Auth, api, wsURL } from "./lib";
import { Ops } from "./ops";
import { Detail, Sessions } from "./sessions";
import { Team } from "./team";

type Tab = "sessions" | "team" | "ops" | "bench";
const TABS: Record<Tab, string> = { sessions: "Sessions", team: "Team", ops: "Ops", bench: "Chaos lab" };

export default function Home() {
  const [auth, setAuth] = useState<Auth | null>(null);
  const [ready, setReady] = useState(false);
  const [loginError, setLoginError] = useState("");
  const [tab, setTab] = useState<Tab>("sessions");
  const [open, setOpen] = useState<string | null>(null);
  const [tick, setTick] = useState(0); // bumped by live events to trigger refetches

  const save = useCallback((a: Auth | null) => {
    try { a ? localStorage.setItem("anton-auth", JSON.stringify(a)) : localStorage.removeItem("anton-auth"); } catch {}
    setAuth(a);
  }, []);

  useEffect(() => {
    // Google sign-in returns the key in the URL fragment; take it and clean the URL.
    const h = new URLSearchParams(location.hash.slice(1));
    if (h.get("token")) {
      save({ token: h.get("token")!, name: h.get("name") || "", team: h.get("team") || "" });
      history.replaceState(null, "", location.pathname);
    } else {
      if (h.get("login_error")) { setLoginError(h.get("login_error")!); history.replaceState(null, "", location.pathname); }
      try { const s = localStorage.getItem("anton-auth"); if (s) setAuth(JSON.parse(s)); } catch {}
    }
    setReady(true);
  }, [save]);

  // Refresh role and team from the server (a member may have been promoted).
  useEffect(() => {
    if (!auth) return;
    api<{ name: string; team: string; role: string }>(auth, "/api/me")
      .then((me) => { if (me.role !== auth.role || me.team !== auth.team) save({ ...auth, ...me }); })
      .catch((e) => { if (e.status === 401) save(null); });
  }, [auth, save]);

  // Live updates: one WebSocket; a 5 s poll covers a dropped connection.
  useEffect(() => {
    if (!auth) return;
    const ws = new WebSocket(wsURL(`/ws?token=${encodeURIComponent(auth.token)}`));
    ws.onmessage = () => setTick((t) => t + 1);
    const poll = setInterval(() => setTick((t) => t + 1), 5000);
    return () => { ws.close(); clearInterval(poll); };
  }, [auth]);

  if (!ready) return null;
  if (!auth) return <Login onLogin={save} error={loginError} />;

  const logout = () => { api(auth, "/api/logout", "POST").catch(() => {}); save(null); };
  return (
    <>
      <header>
        <h1>Anton Platform</h1>
        <nav>
          {(Object.keys(TABS) as Tab[]).map((t) => (
            <button key={t} className={`tab ${tab === t ? "active" : ""}`} onClick={() => { setTab(t); setOpen(null); }}>{TABS[t]}</button>
          ))}
        </nav>
        <span className="spacer muted small">{auth.name} · team {auth.team}{auth.role === "owner" ? " (owner)" : ""}</span>
        <button onClick={logout}>Log out</button>
      </header>
      <main>
        {tab === "sessions" && !open && <Sessions auth={auth} tick={tick} onOpen={setOpen} onAuthLost={() => save(null)} />}
        {tab === "sessions" && open && <Detail auth={auth} name={open} tick={tick} onBack={() => setOpen(null)} />}
        {tab === "team" && <Team auth={auth} tick={tick} />}
        {tab === "ops" && <Ops auth={auth} tick={tick} />}
        {tab === "bench" && <Bench auth={auth} tick={tick} />}
      </main>
    </>
  );
}

function Login({ onLogin, error }: { onLogin: (a: Auth) => void; error: string }) {
  const [cfg, setCfg] = useState<{ google: boolean; dev_login: boolean } | null>(null);
  const [name, setName] = useState("");
  const [team, setTeam] = useState("");
  const [err, setErr] = useState(error);
  useEffect(() => { api<{ google: boolean; dev_login: boolean }>(null, "/api/auth/config").then(setCfg).catch(() => setCfg({ google: false, dev_login: true })); }, []);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try { onLogin(await api<Auth>(null, "/api/login", "POST", { name, team })); } catch (e) { setErr((e as Error).message); }
  };
  const validTeam = /^[a-z0-9-]{2,30}$/.test(team);
  return (
    <main style={{ maxWidth: 420 }}>
      <form className="panel" onSubmit={submit}>
        <h2>Sign in to Anton</h2>
        <div className="row" style={{ marginBottom: 12 }}>
          <input className="grow" aria-label="team" placeholder="team (lowercase letters, digits, dashes)" value={team} onChange={(e) => setTeam(e.target.value)} />
        </div>
        {cfg?.google && (
          <button type="button" className="primary" style={{ width: "100%", marginBottom: 12 }} disabled={!validTeam}
            onClick={() => { location.href = `/api/auth/google/start?team=${encodeURIComponent(team)}`; }}>
            Continue with Google
          </button>
        )}
        {cfg?.dev_login && (
          <>
            <p className="muted small">Development login: pick a user name. No password; for local clusters only.</p>
            <div className="row" style={{ marginBottom: 12 }}>
              <input className="grow" aria-label="user name" placeholder="user name" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <button className={cfg.google ? "" : "primary"} disabled={!name || !validTeam}>Sign in</button>
          </>
        )}
        <p className="muted small">The first person to use a new team name becomes its owner. Joining an existing team with Google needs an invitation.</p>
        {err && <p className="err small">{err}</p>}
      </form>
    </main>
  );
}
