package main

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/kv"
)

func readyReplica(name, node, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: common.Namespace, Labels: map[string]string{"app": common.ReplicaApp}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

// One replica on a cordoned node (being drained), one on a healthy node.
func testRouter(t *testing.T, sessions ...*v1.AgentSession) *router {
	t.Helper()
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = v1.AddToScheme(sch)
	b := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: common.ModeConfigMap, Namespace: common.Namespace},
			Data: map[string]string{"mode": common.ModeAnton}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "leaving"}, Spec: corev1.NodeSpec{Unschedulable: true}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "staying"}},
		readyReplica("r-leaving", "leaving", "127.0.0.1"),
		readyReplica("r-staying", "staying", "127.0.0.1"),
	)
	for _, s := range sessions {
		b = b.WithObjects(s).WithStatusSubresource(s)
	}
	return &router{c: b.Build(), msPerToken: 0.3, concurrency: 4,
		assign: map[string]string{}, seenRV: map[string]string{}, lastTouch: map[string]time.Time{},
		inflight: map[string]int{}, ewmaMs: map[string]float64{}}
}

func TestExistingSessionStaysOnALeavingReplicaWhileItIsAlive(t *testing.T) {
	rt := testRouter(t)
	rt.assign["s-old"] = "r-leaving" // its cache is warm there
	target, _, decision, err := rt.pick(t.Context(), common.ModeAnton, &kv.CompletionRequest{SessionID: "s-old", ContextTokens: 20000}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "r-leaving" || decision != "affinity" {
		t.Fatalf("existing session moved early: %s (%s)", target.Name, decision)
	}
}

func TestNewSessionAvoidsALeavingReplica(t *testing.T) {
	rt := testRouter(t)
	rt.inflight["r-staying"] = 3 // even when the staying replica is busier
	target, _, decision, err := rt.pick(t.Context(), common.ModeAnton, &kv.CompletionRequest{SessionID: "s-new", ContextTokens: 1000}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "r-staying" || decision != "new" {
		t.Fatalf("new session placed on %s (%s)", target.Name, decision)
	}
}

func TestRestoreLandsOnAReplicaThatStays(t *testing.T) {
	s := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-tier", Namespace: common.Namespace, ResourceVersion: "5"},
		Status: v1.AgentSessionStatus{CacheLocation: v1.CacheTier}}
	rt := testRouter(t, s)
	rt.assign["s-tier"] = "r-leaving"
	target, _, decision, err := rt.pick(t.Context(), common.ModeAnton, &kv.CompletionRequest{SessionID: "s-tier", ContextTokens: 5000}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "r-staying" || decision != "restore" {
		t.Fatalf("restore went to %s (%s)", target.Name, decision)
	}
}

// A session whose replica disappeared must not wait for a rescue that the
// guard never started (it decided recomputing is cheaper).
func TestNoWaitForARescueThatIsNotHappening(t *testing.T) {
	s := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-gone", Namespace: common.Namespace},
		Status: v1.AgentSessionStatus{Phase: v1.PhaseActive, Replica: "r-dying"}}
	rt := testRouter(t, s)
	// r-dying still exists as a terminating pod (preStop window)...
	dying := readyReplica("r-dying", "leaving", "127.0.0.1")
	now := metav1.Now()
	dying.DeletionTimestamp, dying.Finalizers = &now, []string{"test/keep"}
	if err := rt.c.Create(t.Context(), dying); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rt.awaitRelease(t.Context(), "s-gone", "r-dying")
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %v for a rescue that is not happening", waited)
	}
}
