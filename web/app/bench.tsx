"use client";

import { useCallback, useEffect, useState } from "react";
import { Bar, BarChart, CartesianGrid, LabelList, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { Auth, api, ms } from "./lib";

type Result = {
  mode: string; requests: number; errors: number; ttft_p50_ms: number; ttft_p95_ms: number;
  recomputed_tokens: number; prefill_gpu_seconds: number; resume_ttft_p50_ms: number; resume_ttft_p95_ms: number;
  affected_sessions: number; lost_kv_state: number; wake_ttft_p50_ms: number; wake_kv_preserved: number;
  workspace_preserved: number; hibernated: number; seeds?: number; gpu_s_per_request_by_seed?: number[];
  sandboxes_disrupted?: number; workspace_lost?: number; spot_affected?: number; spot_rescued?: number;
};
type Run = { id: number; job: string; status: string; started_at: string; launched_by: string; results: Result[] };

// Each mode keeps its color whatever subset a run contains (color follows the
// entity). Validated categorical slots 1-4; CSS vars switch them for dark mode.
const MODES = ["plain", "sandbox", "routing", "anton"];
const modeColor = (m: string) => `var(--series-${MODES.indexOf(m) + 1 || 1})`;

const METRICS: { key: string; label: string; get: (r: Result) => number; fmt: (v: number) => string }[] = [
  { key: "lost", label: "Disrupted sessions that lost KV state", get: (r) => (r.affected_sessions ? (100 * r.lost_kv_state) / r.affected_sessions : 0), fmt: (v) => `${v.toFixed(0)}%` },
  { key: "gpu", label: "Prefill GPU-seconds per request", get: (r) => r.prefill_gpu_seconds / Math.max(1, r.requests - r.errors), fmt: (v) => v.toFixed(2) },
  { key: "tok", label: "Recomputed tokens per request", get: (r) => r.recomputed_tokens / Math.max(1, r.requests - r.errors), fmt: (v) => Math.round(v).toLocaleString() },
  { key: "resume", label: "Resume TTFT after disruption (p50)", get: (r) => r.resume_ttft_p50_ms, fmt: ms },
  { key: "resume95", label: "Resume TTFT after disruption (p95)", get: (r) => r.resume_ttft_p95_ms, fmt: ms },
  { key: "ws", label: "Disrupted sandboxes that lost their workspace", get: (r) => (r.sandboxes_disrupted ? (100 * (r.workspace_lost ?? 0)) / r.sandboxes_disrupted : 0), fmt: (v) => `${v.toFixed(0)}%` },
  { key: "wake", label: "Wake-from-hibernate TTFT (p50)", get: (r) => r.wake_ttft_p50_ms, fmt: ms },
];

const STATUS: Record<string, string> = { succeeded: "Done", failed: "Failed", running: "Running" };

function MetricChart({ results, m }: { results: Result[]; m: (typeof METRICS)[number] }) {
  const data = results.map((r) => ({ mode: r.mode, value: m.get(r) }));
  return (
    <div className="chart">
      <div className="small"><strong>{m.label}</strong> <span className="muted">(lower is better)</span></div>
      <ResponsiveContainer width="100%" height={150}>
        <BarChart data={data} layout="vertical" margin={{ top: 4, right: 56, bottom: 0, left: 0 }} barCategoryGap={6}>
          <CartesianGrid horizontal={false} stroke="var(--line)" />
          <XAxis type="number" hide domain={[0, "auto"]} />
          <YAxis type="category" dataKey="mode" width={64} tickLine={false} axisLine={false} tick={{ fill: "var(--muted)", fontSize: 12 }} />
          <Tooltip cursor={{ fill: "var(--hover)" }} formatter={(v) => [m.fmt(Number(v)), m.label]}
            contentStyle={{ background: "var(--panel)", border: "1px solid var(--line)", color: "var(--ink)" }} />
          <Bar dataKey="value" maxBarSize={22} radius={[0, 4, 4, 0]} isAnimationActive={false}
            shape={(p: any) => <rect x={p.x} y={p.y} width={Math.max(0, p.width)} height={p.height} rx={4} fill={modeColor(p.payload.mode)} />}>
            <LabelList dataKey="value" position="right" formatter={(v: any) => m.fmt(Number(v))} style={{ fill: "var(--ink)", fontSize: 12 }} />
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

function Trend({ auth, tick }: { auth: Auth; tick: number }) {
  const [metric, setMetric] = useState("prefill_gpu_seconds");
  const [rows, setRows] = useState<Record<string, number | string>[]>([]);
  const [modes, setModes] = useState<string[]>([]);
  useEffect(() => {
    Promise.all([
      api<{ samples: { job: string; mode: string; value: number }[] }>(auth, `/api/benchmarks/samples?metric=${metric}`),
      api<{ samples: { job: string; mode: string; value: number }[] }>(auth, `/api/benchmarks/samples?metric=requests`),
    ]).then(([m, req]) => {
      // Per-request where it matters, so runs of different lengths compare.
      const perReq = ["prefill_gpu_seconds", "recomputed_tokens"].includes(metric);
      const reqs = new Map(req.samples.map((s) => [`${s.job}/${s.mode}`, s.value]));
      const byJob = new Map<string, Record<string, number | string>>();
      for (const s of m.samples) {
        const row = byJob.get(s.job) ?? { job: s.job.replace("anton-bench-", "") };
        row[s.mode] = perReq ? s.value / Math.max(1, reqs.get(`${s.job}/${s.mode}`) ?? 1) : s.value;
        byJob.set(s.job, row);
      }
      setRows([...byJob.values()]);
      setModes(MODES.filter((x) => m.samples.some((s) => s.mode === x)));
    }).catch(() => {});
  }, [auth, metric, tick]);
  if (rows.length < 2) return null;
  return (
    <div className="panel">
      <div className="row">
        <h2 style={{ margin: 0 }}>Across runs</h2>
        <select className="spacer" aria-label="metric" value={metric} onChange={(e) => setMetric(e.target.value)}>
          <option value="prefill_gpu_seconds">Prefill GPU-seconds per request</option>
          <option value="recomputed_tokens">Recomputed tokens per request</option>
          <option value="lost_kv_state">Sessions that lost KV state</option>
          <option value="resume_ttft_p50_ms">Resume TTFT p50 (ms)</option>
          <option value="wake_ttft_p50_ms">Wake TTFT p50 (ms)</option>
        </select>
      </div>
      <ResponsiveContainer width="100%" height={220}>
        <LineChart data={rows} margin={{ top: 12, right: 24, bottom: 0, left: 0 }}>
          <CartesianGrid vertical={false} stroke="var(--line)" />
          <XAxis dataKey="job" tick={{ fill: "var(--muted)", fontSize: 11 }} tickLine={false} />
          <YAxis tick={{ fill: "var(--muted)", fontSize: 11 }} tickLine={false} axisLine={false} width={56} />
          <Tooltip contentStyle={{ background: "var(--panel)", border: "1px solid var(--line)", color: "var(--ink)" }} />
          <Legend />
          {modes.map((m) => <Line key={m} dataKey={m} stroke={modeColor(m)} strokeWidth={2} dot={{ r: 4 }} isAnimationActive={false} />)}
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}

export function Bench({ auth, tick }: { auth: Auth; tick: number }) {
  const [runs, setRuns] = useState<Run[]>([]);
  const [duration, setDuration] = useState("3m");
  const [seeds, setSeeds] = useState(1);
  const [notice, setNotice] = useState("10s");
  const [err, setErr] = useState("");
  const [armed, setArmed] = useState(false);
  const load = useCallback(() => { api<{ runs: Run[] }>(auth, "/api/benchmarks").then((r) => setRuns(r.runs)).catch((e) => setErr(e.message)); }, [auth]);
  useEffect(load, [load, tick]);
  const running = runs.some((r) => r.status === "running");
  // Two-step button instead of confirm(): some embedded browsers dismiss dialogs.
  const launch = () => {
    if (!armed) { setArmed(true); return; }
    setArmed(false);
    api(auth, "/api/benchmarks", "POST", { duration, sessions: 24, seeds, spot_deadline: notice }).then(load).catch((e) => setErr(e.message));
  };
  return (
    <>
      <div className="panel">
        <h2>Chaos benchmark</h2>
        <p className="muted small">
          Runs the same 24-session workload in four modes (plain Kubernetes, sandbox persistence only, cache-affinity routing only, Anton)
          and hits each with a node drain, a spot interruption, a consolidation and a scale-down, then hibernates and wakes four sessions.
        </p>
        <div className="row">
          <label className="small muted">Load per mode{" "}
            <select value={duration} onChange={(e) => setDuration(e.target.value)}>
              <option value="1m">1 minute</option><option value="3m">3 minutes</option><option value="5m">5 minutes</option>
            </select>
          </label>
          <label className="small muted">Seeds{" "}
            <select value={seeds} onChange={(e) => setSeeds(+e.target.value)}>
              {[1, 2, 3, 5].map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </label>
          <label className="small muted">Spot notice{" "}
            <select value={notice} onChange={(e) => setNotice(e.target.value)}>
              <option value="10s">10 s (forces triage)</option><option value="30s">30 s</option><option value="120s">2 min (AWS)</option>
            </select>
          </label>
          <button className={armed ? "danger" : "primary"} disabled={running} onClick={launch} onBlur={() => setArmed(false)}>
            {running ? "Running…" : armed ? "Confirm: deletes all sessions and disrupts nodes" : "Run benchmark"}
          </button>
        </div>
        {err && <p className="err small">{err}</p>}
      </div>
      <Trend auth={auth} tick={tick} />
      {runs.map((run) => (
        <div className="panel" key={run.id}>
          <h2>
            {run.job} <span className={`badge ${run.status === "succeeded" ? "Active" : run.status === "failed" ? "Failed" : "Pending"}`}>{STATUS[run.status] ?? run.status}</span>{" "}
            <span className="muted small">{new Date(run.started_at).toLocaleString()}{run.launched_by ? ` · by ${run.launched_by}` : ""}
              {run.results[0]?.seeds && run.results[0].seeds > 1 ? ` · ${run.results[0].seeds} seeds, counts summed, latencies averaged` : ""}</span>
          </h2>
          {run.results.length === 0 ? <p className="muted">No results yet.</p> : (
            <>
              <div className="charts">{METRICS.map((m) => <MetricChart key={m.key} results={run.results} m={m} />)}</div>
              <details>
                <summary className="small muted">Table view</summary>
                <div className="tablewrap">
                  <table>
                    <thead><tr><th>Metric (lower is better)</th>{run.results.map((r) => <th key={r.mode}>{r.mode}</th>)}</tr></thead>
                    <tbody>
                      {METRICS.map((m) => <tr key={m.key}><td>{m.label}</td>{run.results.map((r) => <td key={r.mode}>{m.fmt(m.get(r))}</td>)}</tr>)}
                      <tr><td>Disrupted sessions that lost KV state</td>{run.results.map((r) => <td key={r.mode}>{r.lost_kv_state} of {r.affected_sessions}</td>)}</tr>
                      <tr><td>Spot: KV caches that survived the notice</td>{run.results.map((r) => <td key={r.mode}>{r.spot_rescued ?? 0} of {r.spot_affected ?? 0}</td>)}</tr>
                      <tr><td>Disrupted sandboxes that lost their workspace</td>{run.results.map((r) => <td key={r.mode}>{r.workspace_lost ?? 0} of {r.sandboxes_disrupted ?? 0}</td>)}</tr>
                      {(run.results[0]?.seeds ?? 1) > 1 && <tr><td>GPU-s per request, by seed</td>{run.results.map((r) => <td key={r.mode}>{(r.gpu_s_per_request_by_seed ?? []).map((v) => v.toFixed(2)).join(" · ")}</td>)}</tr>}
                      <tr><td>Workspace preserved across hibernate</td>{run.results.map((r) => <td key={r.mode}>{r.workspace_preserved} of {r.hibernated}</td>)}</tr>
                      <tr><td>KV cache preserved across hibernate</td>{run.results.map((r) => <td key={r.mode}>{r.wake_kv_preserved} of {r.hibernated}</td>)}</tr>
                      <tr><td>Requests (errors)</td>{run.results.map((r) => <td key={r.mode}>{r.requests} ({r.errors})</td>)}</tr>
                    </tbody>
                  </table>
                </div>
              </details>
            </>
          )}
        </div>
      ))}
    </>
  );
}
