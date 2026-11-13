package controller

import (
	"strings"
	"testing"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
)

// The hibernation decision on the measured hardware: Qwen2.5-7B with vLLM +
// LMCache moves a 16k cache on an L4 but recomputes it on an A100, where
// prefill is 4x faster and the tier is no faster.
func TestHibernateMoveFollowsMeasuredHardware(t *testing.T) {
	session := kv.SessionCache{SessionID: "s", Tokens: 16000, Bytes: 16000 * 57344}
	l4 := &kv.ReplicaInfo{
		PrefillCurve:       "1000:0.2475,2000:0.4922,4000:1.0277,8000:2.1659,16000:4.8260,32000:11.7153",
		OffloadBytesPerSec: 1.283e9, RestoreBytesPerSec: 0.627e9, Sessions: []kv.SessionCache{session},
	}
	a100 := &kv.ReplicaInfo{
		PrefillCurve:       "1000:0.0820,2000:0.1259,4000:0.2544,8000:0.5257,16000:1.1685,32000:2.9245",
		OffloadBytesPerSec: 1.250e9, RestoreBytesPerSec: 0.651e9, Sessions: []kv.SessionCache{session},
	}
	if move, why := hibernateMove(l4, "s"); !move {
		t.Fatalf("L4: should move the cache, got %q", why)
	}
	move, why := hibernateMove(a100, "s")
	if move || !strings.Contains(why, "cheaper than moving") {
		t.Fatalf("A100: should recompute, got move=%v %q", move, why)
	}
	if move, _ := hibernateMove(l4, "other"); move {
		t.Fatal("no cache for the session: nothing to move")
	}
}
