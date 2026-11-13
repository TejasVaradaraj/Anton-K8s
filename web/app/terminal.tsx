"use client";

// A live shell in the session's sandbox: xterm.js in the browser, a WebSocket
// to the API, and a Kubernetes exec into the sandbox pod on the other side.

import { useEffect, useRef, useState } from "react";
import "@xterm/xterm/css/xterm.css";
import { Auth, wsURL } from "./lib";

export default function Terminal({ auth, session, running }: { auth: Auth; session: string; running: boolean }) {
  const host = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState("connecting…");

  useEffect(() => {
    if (!running || !host.current) return;
    let disposed = false;
    let cleanup = () => {};
    (async () => {
      // xterm touches `window`, so load it only in the browser.
      const [{ Terminal: XTerm }, { FitAddon }] = await Promise.all([import("@xterm/xterm"), import("@xterm/addon-fit")]);
      if (disposed || !host.current) return;
      const css = getComputedStyle(document.documentElement);
      const term = new XTerm({
        cursorBlink: true, fontSize: 13, convertEol: false,
        fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
        theme: { background: css.getPropertyValue("--term-bg").trim() || "#0f1115", foreground: css.getPropertyValue("--term-fg").trim() || "#e8eaee" },
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(host.current);
      fit.fit();

      const ws = new WebSocket(wsURL(`/api/sessions/${session}/terminal?token=${encodeURIComponent(auth.token)}`));
      const send = (m: object) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify(m));
      ws.onopen = () => { setStatus("connected"); send({ type: "resize", cols: term.cols, rows: term.rows }); term.focus(); };
      ws.onmessage = (e) => term.write(typeof e.data === "string" ? e.data : "");
      ws.onclose = (e) => setStatus(e.code === 4409 ? "sandbox not running" : e.code === 4404 ? "not allowed" : "disconnected");
      const input = term.onData((data) => send({ type: "input", data }));
      const onResize = () => { fit.fit(); send({ type: "resize", cols: term.cols, rows: term.rows }); };
      const ro = new ResizeObserver(onResize);
      ro.observe(host.current);
      cleanup = () => { ro.disconnect(); input.dispose(); ws.close(); term.dispose(); };
    })();
    return () => { disposed = true; cleanup(); };
  }, [auth.token, session, running]);

  if (!running) return <p className="muted">The sandbox is not running. Send a message or press Resume to wake it.</p>;
  return (
    <>
      <div className="term" ref={host} />
      <p className="muted small">Shell in the sandbox ({status}). Opening a terminal is recorded in the team audit log.</p>
    </>
  );
}
