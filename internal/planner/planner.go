// Package planner holds the three decision rules that are the core of Anton:
//
//  1. the offload-vs-recompute cost model (Worth),
//  2. the cache-value score used to pick scale-down victims (Value, ReplicaScore),
//  3. the deadline-constrained rescue plan (PlanGreedy, with PlanDP as the
//     optimal reference it is compared against).
//
// It is pure Go with no Kubernetes imports so it can be unit-tested and
// replaced without touching the operator.
package planner

import (
	"math"
	"sort"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/prefill"
)

type Candidate struct {
	ID         string
	Tokens     int64
	Bytes      int64
	Priority   int32
	LastAccess time.Time
	Hits       int
}

type Params struct {
	PrefillSecPerToken float64        // measured: seconds of GPU prefill per token (used when Curve is nil)
	Curve              *prefill.Curve // measured prefill time by context length (preferred: superlinear)
	OffloadBps         float64        // measured: bytes/sec replica -> cache tier
	RestoreBps         float64        // measured: bytes/sec cache tier -> replica
	HalfLife           time.Duration  // how fast an untouched session's reuse chance decays
}

func (p Params) PrefillSec(tokens int64) float64 {
	if p.Curve != nil {
		return p.Curve.Seconds(tokens)
	}
	return float64(tokens) * p.PrefillSecPerToken
}
func (p Params) OffloadSec(bytes int64) float64 { return float64(bytes) / p.OffloadBps }
func (p Params) RestoreSec(bytes int64) float64 { return float64(bytes) / p.RestoreBps }

// Worth is the offload-vs-recompute rule: moving KV blocks only pays off when
// moving them out and back is faster than recomputing them on a GPU.
func Worth(c Candidate, p Params) bool {
	return p.OffloadSec(c.Bytes)+p.RestoreSec(c.Bytes) < p.PrefillSec(c.Tokens)
}

// reuseProb estimates the chance the session sends another request soon:
// exponential decay on recency, nudged up by how often its prefix was reused.
func reuseProb(c Candidate, p Params, now time.Time) float64 {
	half := p.HalfLife
	if half <= 0 {
		half = 5 * time.Minute
	}
	age := now.Sub(c.LastAccess)
	if age < 0 {
		age = 0
	}
	recency := math.Pow(0.5, age.Seconds()/half.Seconds())
	hitRate := float64(c.Hits) / float64(c.Hits+1)
	return recency * (0.5 + 0.5*hitRate)
}

// Value is the expected GPU-seconds of prefill this cache entry avoids.
func Value(c Candidate, p Params, now time.Time) float64 {
	return reuseProb(c, p, now) * p.PrefillSec(c.Tokens)
}

// netSaving is what a rescue actually buys: prefill avoided minus restore paid.
func netSaving(c Candidate, p Params, now time.Time) float64 {
	return reuseProb(c, p, now) * (p.PrefillSec(c.Tokens) - p.RestoreSec(c.Bytes))
}

// ReplicaScore ranks replicas for scale-down: the lowest score is the cheapest
// replica to lose. It is written to controller.kubernetes.io/pod-deletion-cost.
func ReplicaScore(cs []Candidate, p Params, now time.Time) float64 {
	var s float64
	for _, c := range cs {
		s += Value(c, p, now)
	}
	return s
}

type Skip struct {
	ID     string
	Reason string
}

type Plan struct {
	Chosen     []Candidate // in the order they should be offloaded
	Skipped    []Skip
	SavedSec   float64
	MovedBytes int64
}

// PlanGreedy picks which sessions to offload before a deadline. Offloads from
// one replica share its outbound bandwidth, so the budget is a byte capacity:
// a knapsack with value = net prefill saved and weight = bytes to move.
// Higher priority tiers are served first; inside a tier, best value per byte.
func PlanGreedy(cs []Candidate, p Params, budget time.Duration, now time.Time) Plan {
	var plan Plan
	var ok []Candidate
	for _, c := range cs {
		if c.Tokens <= 0 || c.Bytes <= 0 {
			continue
		}
		if !Worth(c, p) {
			plan.Skipped = append(plan.Skipped, Skip{c.ID, "recompute is cheaper than moving"})
			continue
		}
		ok = append(ok, c)
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].Priority != ok[j].Priority {
			return ok[i].Priority > ok[j].Priority
		}
		return netSaving(ok[i], p, now)/float64(ok[i].Bytes) > netSaving(ok[j], p, now)/float64(ok[j].Bytes)
	})
	remaining := int64(math.Max(0, budget.Seconds()) * p.OffloadBps)
	for _, c := range ok {
		if c.Bytes > remaining {
			plan.Skipped = append(plan.Skipped, Skip{c.ID, "does not fit before the deadline"})
			continue
		}
		remaining -= c.Bytes
		plan.Chosen = append(plan.Chosen, c)
		plan.SavedSec += netSaving(c, p, now)
		plan.MovedBytes += c.Bytes
	}
	return plan
}

// PlanDP is the optimal 0/1 knapsack (ignoring priority tiers), with weights
// rounded up to 16 MiB units. It is too slow to trust under a real deadline
// with many sessions, so it is only the yardstick greedy is measured against.
func PlanDP(cs []Candidate, p Params, budget time.Duration, now time.Time) Plan {
	const unit = 16 << 20
	var ok []Candidate
	for _, c := range cs {
		if c.Tokens > 0 && c.Bytes > 0 && Worth(c, p) {
			ok = append(ok, c)
		}
	}
	capUnits := int(int64(math.Max(0, budget.Seconds())*p.OffloadBps) / unit)
	n := len(ok)
	best := make([][]float64, n+1)
	for i := range best {
		best[i] = make([]float64, capUnits+1)
	}
	w := make([]int, n)
	for i, c := range ok {
		w[i] = int((c.Bytes + unit - 1) / unit)
		v := netSaving(c, p, now)
		for u := 0; u <= capUnits; u++ {
			best[i+1][u] = best[i][u]
			if w[i] <= u && best[i][u-w[i]]+v > best[i+1][u] {
				best[i+1][u] = best[i][u-w[i]] + v
			}
		}
	}
	var plan Plan
	u := capUnits
	for i := n; i > 0; i-- {
		if best[i][u] != best[i-1][u] {
			c := ok[i-1]
			plan.Chosen = append(plan.Chosen, c)
			plan.SavedSec += netSaving(c, p, now)
			plan.MovedBytes += c.Bytes
			u -= w[i-1]
		}
	}
	return plan
}
