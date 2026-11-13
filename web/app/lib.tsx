// Shared helpers: the API client, types and small presentational bits.

export type Auth = { token: string; name: string; team: string; role?: string };

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
  }
}

export async function api<T>(auth: Auth | null, path: string, method = "GET", body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", ...(auth ? { Authorization: `Bearer ${auth.token}` } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const detail = await res.json().then((j) => j.detail).catch(() => res.statusText);
    throw new ApiError(typeof detail === "string" ? detail : JSON.stringify(detail), res.status);
  }
  return res.json();
}

export function wsURL(path: string): string {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return `${proto}://${location.host}${path}`;
}

export type Session = {
  name: string; phase: string; desired: string; node: string; replica: string; priority: number;
  cache_location: string; cached_tokens: number; context_tokens: number; message: string;
  hibernations: number; rescues: number; owner: string; template_id: number | null;
};
export type LiveEvent = { id: number; session?: string; kind: string; detail: string; at: string };
export type Msg = {
  id: number; role: string; content: string; ttft_ms: number | null; cached_tokens: number | null;
  recomputed_tokens: number | null; replica: string | null; at: string;
};
export type Template = { id: number; name: string; description: string; priority: number; idle_timeout_seconds: number; builtin: boolean };

export const Badge = ({ v }: { v: string }) => <span className={`badge ${v}`}>{v || "-"}</span>;
export const ms = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${Math.round(v)} ms`);
export const time = (iso: string) => new Date(iso).toLocaleTimeString();
export const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
export const idleLabel = (s: number) => (s === 0 ? "never hibernates" : `hibernates after ${s >= 60 ? `${Math.round(s / 60)} min` : `${s} s`} idle`);
