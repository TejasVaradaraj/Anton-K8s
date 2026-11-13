// Package metrics defines Anton's Prometheus metrics. They are registered in
// controller-runtime's registry, so the operator serves them on its existing
// metrics endpoint and the router serves the same registry at /metrics.
package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
)

var (
	// Operator.
	Hibernations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anton_hibernations_total", Help: "Completed hibernations by where the KV cache ended up.",
	}, []string{"cache_location"})
	HibernateSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "anton_hibernate_duration_seconds", Help: "Time from Hibernating to Hibernated.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	})

	// Disruption guard.
	RescuePlans = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "anton_rescue_plans_total", Help: "Rescue plans made for replicas about to disappear.",
	})
	RescueSessions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anton_rescue_sessions_total",
		Help: "Sessions on a disappearing replica, by outcome: saved, failed (offload error or deadline), skipped (planner chose not to move).",
	}, []string{"outcome"})
	RescueBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "anton_rescue_bytes_total", Help: "KV-cache bytes moved to the tier by rescues.",
	})
	RescueBudgetSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "anton_rescue_budget_seconds", Help: "Time left before the deadline when a rescue plan was made.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120},
	})
	ReplicaCacheValue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "anton_replica_cache_value", Help: "Cache value the guard computed for a replica (written as pod-deletion-cost).",
	}, []string{"replica"})

	// Router.
	RouterRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anton_router_requests_total",
		Help: "Routed requests by mode and placement decision: random, new, affinity, affinity_broken, restore, error.",
	}, []string{"mode", "decision"})
	RouterRestoreSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "anton_router_restore_seconds", Help: "KV restore time before the first token.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8},
	})
	RouterWakeWaitSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "anton_router_wake_wait_seconds", Help: "Time a request waited for its session to become Active.",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60},
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(Hibernations, HibernateSeconds, RescuePlans, RescueSessions, RescueBytes,
		RescueBudgetSeconds, ReplicaCacheValue, RouterRequests, RouterRestoreSeconds, RouterWakeWaitSeconds)
}

// SessionCollector reports session counts by phase and cached tokens by
// cache location, read from the manager's cache on every scrape.
type SessionCollector struct {
	Reader client.Reader
}

var (
	sessionsDesc = prometheus.NewDesc("anton_sessions", "AgentSessions by phase.", []string{"phase"}, nil)
	tokensDesc   = prometheus.NewDesc("anton_session_cached_tokens", "Cached context tokens by cache location.", []string{"location"}, nil)
)

func (c SessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sessionsDesc
	ch <- tokensDesc
}

func (c SessionCollector) Collect(ch chan<- prometheus.Metric) {
	var list v1.AgentSessionList
	if err := c.Reader.List(context.Background(), &list, client.InNamespace(common.Namespace)); err != nil {
		return
	}
	phases := map[string]float64{}
	for _, p := range []string{v1.PhasePending, v1.PhaseActive, v1.PhaseHibernating, v1.PhaseHibernated,
		v1.PhaseWaking, v1.PhaseMigrating, v1.PhaseRescuing, v1.PhaseFailed} {
		phases[p] = 0
	}
	tokens := map[string]float64{v1.CacheReplica: 0, v1.CacheTier: 0, v1.CacheNone: 0}
	for _, s := range list.Items {
		phases[s.Status.Phase]++
		if loc := s.Status.CacheLocation; loc != "" {
			tokens[loc] += float64(s.Status.CachedTokens)
		}
	}
	for p, n := range phases {
		if p != "" {
			ch <- prometheus.MustNewConstMetric(sessionsDesc, prometheus.GaugeValue, n, p)
		}
	}
	for l, n := range tokens {
		ch <- prometheus.MustNewConstMetric(tokensDesc, prometheus.GaugeValue, n, l)
	}
}
