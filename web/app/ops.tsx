"use client";

import { useCallback, useEffect, useState } from "react";
import { Auth, Badge, api, plural, time } from "./lib";

type OpsData = {
  mode: string; total_sessions: number; phases: Record<string, number>;
  nodes: { name: string; control_plane: boolean; cordoned: boolean; spot_deadline: string | null; sandboxes: number }[];
  replicas: { name: string; node: string; ready: boolean; terminating: boolean; deletion_cost: string | null; sessions: number }[];
};

export function Ops({ auth, tick }: { auth: Auth; tick: number }) {
  const [d, setD] = useState<OpsData | null>(null);
  const [err, setErr] = useState("");
  const load = useCallback(() => { api<OpsData>(auth, "/api/ops").then(setD).catch((e) => setErr(e.message)); }, [auth]);
  useEffect(load, [load, tick]);
  if (!d) return <p className="muted">{err || "Loading…"}</p>;
  return (
    <>
      <div className="panel">
        <div className="row">
          <h2 style={{ margin: 0 }}>Cluster</h2>
          <span className="spacer small muted">Mode</span>
          <select aria-label="mode" value={d.mode} disabled={auth.role !== "owner"} title={auth.role !== "owner" ? "team owners only" : ""}
            onChange={(e) => api(auth, "/api/mode", "PUT", { mode: e.target.value }).then(load).catch((x) => setErr(x.message))}>
            <option value="anton">anton (routing + rescue)</option>
            <option value="routing">routing only (llm-d style)</option>
            <option value="sandbox">sandbox persistence only (Agent Sandbox style)</option>
            <option value="plain">plain Kubernetes</option>
          </select>
        </div>
        <div className="cards" style={{ marginTop: 12 }}>
          <div className="card"><div className="muted small">Sessions</div><div className="stat">{d.total_sessions}</div></div>
          {Object.entries(d.phases).map(([p, n]) => (
            <div className="card" key={p}><div className="small"><Badge v={p} /></div><div className="stat">{n}</div></div>
          ))}
        </div>
        {err && <p className="err small">{err}</p>}
      </div>
      <div className="panel">
        <h2>Nodes</h2>
        <div className="cards">
          {d.nodes.map((n) => (
            <div key={n.name} className={`card ${n.cordoned || n.spot_deadline ? "risk" : ""}`}>
              <strong>{n.name}</strong>
              <div className="small muted">{n.control_plane ? "control plane" : "worker"}</div>
              <div className="small">{plural(n.sandboxes, "sandbox", "sandboxes")} · {plural(d.replicas.filter((r) => r.node === n.name).length, "replica", "replicas")}</div>
              {n.spot_deadline && <div className="small err">spot notice, gone at {time(n.spot_deadline)}</div>}
              {n.cordoned && <div className="small err">cordoned</div>}
            </div>
          ))}
        </div>
      </div>
      <div className="panel">
        <h2>Inference replicas</h2>
        <div className="tablewrap">
          <table>
            <thead><tr><th>Replica</th><th>Node</th><th>Status</th><th className="num">Sessions</th><th className="num">Deletion cost</th></tr></thead>
            <tbody>
              {d.replicas.map((r) => (
                <tr key={r.name}>
                  <td>{r.name}</td><td className="muted">{r.node}</td>
                  <td><Badge v={r.terminating ? "Rescuing" : r.ready ? "Active" : "Pending"} /></td>
                  <td className="num">{r.sessions}</td><td className="num">{r.deletion_cost ?? "-"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="muted small">Deletion cost is the cache value the guard computed; on scale-down Kubernetes removes the lowest first.</p>
      </div>
    </>
  );
}
