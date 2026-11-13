"""Per-principal request limits: a fixed one-minute window counted in Redis,
so every API replica shares the same count. Without REDIS_URL (tests, local
runs) the count lives in this process, which is only correct for one replica.

If Redis is unreachable the limiter fails open: rate limiting protects the
cluster from noisy clients, and losing it briefly is better than an outage."""
import logging
import os
import threading
import time

log = logging.getLogger("ratelimit")
REDIS_URL = os.environ.get("REDIS_URL", "")


class Limiter:
    def __init__(self, url: str = REDIS_URL):
        self._redis = None
        if url:
            import redis
            self._redis = redis.Redis.from_url(url, socket_timeout=0.2, socket_connect_timeout=0.2)
        self._local: dict[str, int] = {}
        self._lock = threading.Lock()

    def hit(self, key: str, limit: int, now: float | None = None) -> tuple[bool, int]:
        """Counts one request. Returns (allowed, seconds until the window resets)."""
        now = time.time() if now is None else now
        window = int(now // 60)
        retry = 60 - int(now % 60)
        bucket = f"rl:{key}:{window}"
        if self._redis is not None:
            try:
                pipe = self._redis.pipeline()
                pipe.incr(bucket)
                pipe.expire(bucket, 120)
                count = pipe.execute()[0]
                return count <= limit, retry
            except Exception as e:  # noqa: BLE001 - fail open, see module docstring
                log.warning("redis unavailable, not limiting: %s", e)
                return True, retry
        with self._lock:
            for k in [k for k in self._local if not k.endswith(f":{window}")]:
                del self._local[k]
            self._local[bucket] = self._local.get(bucket, 0) + 1
            return self._local[bucket] <= limit, retry


limiter = Limiter()
