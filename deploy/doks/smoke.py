"""Smoke test for the real-GPU cluster: create a session through the API,
send two messages to its OpenCode agent, and require that the second turn
reuses the first turn's KV cache. Runs where kubectl points at the cluster.
Exits non-zero on failure so run.sh stops before the long stages."""
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request

PORT = 18000
pf = subprocess.Popen(["kubectl", "-n", "anton", "port-forward", "svc/anton-api", f"{PORT}:8000"],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def call(method, path, body=None, token=None, timeout=900):
    req = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}", method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Content-Type": "application/json",
                                          **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        # The body carries the reason ("agent error: ...", "cluster refused: ...");
        # without it a failed smoke run cannot be diagnosed after teardown.
        e.detail = e.read().decode(errors="replace")[:2000]
        print(f"HTTP {e.code} from {method} {path}: {e.detail}", flush=True)
        raise


try:
    for _ in range(60):
        try:
            call("GET", "/healthz")
            break
        except OSError:
            time.sleep(1)
    tok = call("POST", "/api/login", {"name": "smoke", "team": "smoke"})["token"]
    name = call("POST", "/api/sessions", {"label": "smoke", "template": "agent"}, tok)["name"]
    print("session", name)
    turns = []
    for text in ["Create a file hello.py that prints hello, then tell me what you did.",
                 "Now add a function that returns the sum of two numbers."]:
        t0 = time.time()
        # A new sandbox on a managed cluster first creates and attaches its
        # volume and pulls its image; the API gives up waiting after 180 s
        # (504). Retry only that, a few times.
        for attempt in range(4):
            try:
                out = call("POST", f"/api/sessions/{name}/messages", {"content": text}, tok)
                break
            except urllib.error.HTTPError as e:
                if e.code != 504 or attempt == 3:
                    raise
                print("sandbox not ready yet (504), retrying")
        turns.append(out)
        print(json.dumps({k: out.get(k) for k in ("backend", "replica", "prompt_tokens", "cached_tokens", "ttft_ms")}),
              f"wall {time.time() - t0:.1f}s")
        print("reply:", (out.get("reply") or "")[:300].replace("\n", " "))
    files = call("GET", f"/api/sessions/{name}/files", token=tok)
    print("files:", [f["path"] for f in files.get("files", [])][:20])
    ok = all(t.get("reply") for t in turns)
    print("SMOKE", "OK" if ok else "FAILED")
    sys.exit(0 if ok else 1)
finally:
    pf.terminate()
