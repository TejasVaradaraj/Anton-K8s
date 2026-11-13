package planner

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/prefill"
)

var now = time.Unix(1_800_000_000, 0)

// 0.4 ms/token prefill, 100 KB/token, 1.5 GB/s out, 2 GB/s back.
var params = Params{PrefillSecPerToken: 0.0004, OffloadBps: 1.5e9, RestoreBps: 2e9, HalfLife: 5 * time.Minute}

func cand(id string, tokens int64, prio int32, age time.Duration) Candidate {
	return Candidate{ID: id, Tokens: tokens, Bytes: tokens * 100_000, Priority: prio, LastAccess: now.Add(-age), Hits: 3}
}

func TestWorth(t *testing.T) {
	if !Worth(cand("a", 30000, 0, 0), params) {
		t.Fatal("fast link: offload should beat recompute")
	}
	slow := params
	slow.OffloadBps, slow.RestoreBps = 1e8, 1e8
	if Worth(cand("a", 30000, 0, 0), slow) {
		t.Fatal("slow link: recompute should beat offload")
	}
}

func TestGreedyRespectsDeadline(t *testing.T) {
	cs := []Candidate{cand("a", 40000, 0, 0), cand("b", 30000, 0, 0), cand("c", 20000, 0, 0), cand("d", 10000, 0, 0)}
	budget := 4 * time.Second // 6 GB of capacity; candidates total 10 GB
	plan := PlanGreedy(cs, params, budget, now)
	if got := params.OffloadSec(plan.MovedBytes); got > budget.Seconds() {
		t.Fatalf("plan needs %.1fs but budget is %v", got, budget)
	}
	if len(plan.Chosen) == 0 || len(plan.Chosen) == len(cs) {
		t.Fatalf("expected a partial rescue, got %d of %d", len(plan.Chosen), len(cs))
	}
	if len(plan.Chosen)+len(plan.Skipped) != len(cs) {
		t.Fatal("every candidate must be either chosen or skipped")
	}
}

func TestGreedyPriorityFirst(t *testing.T) {
	cs := []Candidate{cand("low-hot", 30000, 0, 0), cand("high-cold", 30000, 10, 4*time.Minute)}
	plan := PlanGreedy(cs, params, 2*time.Second, now) // room for exactly one
	if len(plan.Chosen) != 1 || plan.Chosen[0].ID != "high-cold" {
		t.Fatalf("priority tier must win, got %+v", plan.Chosen)
	}
}

func TestGreedyPrefersRecentWithinTier(t *testing.T) {
	cs := []Candidate{cand("stale", 30000, 0, 20*time.Minute), cand("fresh", 30000, 0, 0)}
	plan := PlanGreedy(cs, params, 2*time.Second, now)
	if len(plan.Chosen) != 1 || plan.Chosen[0].ID != "fresh" {
		t.Fatalf("want fresh session rescued, got %+v", plan.Chosen)
	}
}

func TestZeroBudgetRescuesNothing(t *testing.T) {
	plan := PlanGreedy([]Candidate{cand("a", 30000, 0, 0)}, params, 0, now)
	if len(plan.Chosen) != 0 {
		t.Fatal("nothing fits in a zero budget")
	}
}

func TestReplicaScoreColdIsLower(t *testing.T) {
	hot := []Candidate{cand("a", 40000, 0, 0), cand("b", 30000, 0, 0)}
	cold := []Candidate{cand("c", 40000, 0, 30*time.Minute)}
	if ReplicaScore(cold, params, now) >= ReplicaScore(hot, params, now) {
		t.Fatal("the cold replica must be the cheaper victim")
	}
}

// Greedy is not optimal, so measure how close it gets to the DP optimum.
func TestGreedyCloseToOptimal(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	worst := 1.0
	for trial := 0; trial < 200; trial++ {
		var cs []Candidate
		for i := 0; i < 12; i++ {
			cs = append(cs, cand(fmt.Sprint(i), 5000+rng.Int63n(45000), 0, time.Duration(rng.Intn(600))*time.Second))
		}
		budget := time.Duration(2+rng.Intn(10)) * time.Second
		g, d := PlanGreedy(cs, params, budget, now), PlanDP(cs, params, budget, now)
		if d.SavedSec > 0 && g.SavedSec/d.SavedSec < worst {
			worst = g.SavedSec / d.SavedSec
		}
	}
	t.Logf("worst greedy/optimal ratio over 200 random cases: %.3f", worst)
	if worst < 0.5 {
		t.Fatalf("greedy fell to %.2f of optimal", worst)
	}
}

// The offload-or-recompute rule on real measurements (vLLM 0.33 + LMCache
// 0.5.6 on an L4, bench/calibration_vllm_*.json): for Qwen2.5-7B moving a
// 16k-token cache beats recomputing it; for Qwen2.5-1.5B, whose prefill is
// cheap per byte of KV, recomputing wins.
func TestWorthOnMeasuredModels(t *testing.T) {
	q7 := Params{
		Curve:      prefill.MustParse("1000:0.2475,2000:0.4922,4000:1.0277,8000:2.1659,16000:4.8260,32000:11.7153"),
		OffloadBps: 1.283e9, RestoreBps: 0.627e9,
	}
	q15 := Params{
		Curve:      prefill.MustParse("1000:0.0535,2000:0.1130,4000:0.2427,8000:0.5469,16000:1.3129"),
		OffloadBps: 0.712e9, RestoreBps: 0.451e9,
	}
	if !Worth(Candidate{Tokens: 16000, Bytes: 16000 * 57344}, q7) {
		t.Error("7B, 16k tokens: moving the cache should be worth it")
	}
	if !Worth(Candidate{Tokens: 1000, Bytes: 1000 * 57344}, q7) {
		t.Error("7B, 1k tokens: moving should still be worth it (0.81 of prefill)")
	}
	if Worth(Candidate{Tokens: 16000, Bytes: 16000 * 28672}, q15) {
		t.Error("1.5B, 16k tokens: recomputing should win")
	}
}
