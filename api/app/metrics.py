"""Prometheus metrics for the product API, served at /metrics.

Request metrics are labelled by route template (/api/sessions/{name}), never
by raw path, so session names cannot blow up label cardinality."""
import time

from fastapi import FastAPI, Request
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from starlette.responses import Response

REQUESTS = Counter("anton_api_requests_total", "API requests", ["method", "route", "status"])
LATENCY = Histogram("anton_api_request_seconds", "API request latency", ["method", "route"])
CHAT_TTFT = Histogram("anton_chat_ttft_seconds", "Time to first token seen by the API for chat turns",
                      buckets=(0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32))
CHAT_TOKENS = Counter("anton_chat_tokens_total", "Prompt tokens by whether they came from the KV cache", ["source"])


def observe_chat(ttft_seconds: float, cached: int, recomputed: int) -> None:
    CHAT_TTFT.observe(ttft_seconds)
    CHAT_TOKENS.labels("cached").inc(cached)
    CHAT_TOKENS.labels("recomputed").inc(recomputed)


def instrument(app: FastAPI) -> None:
    @app.middleware("http")
    async def _measure(request: Request, call_next):
        t0 = time.perf_counter()
        response = await call_next(request)
        route = request.scope.get("route")
        path = getattr(route, "path", None)
        if path and path.startswith("/api"):
            REQUESTS.labels(request.method, path, str(response.status_code)).inc()
            LATENCY.labels(request.method, path).observe(time.perf_counter() - t0)
        return response

    @app.get("/metrics", include_in_schema=False)
    def _metrics():
        return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)
