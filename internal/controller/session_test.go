package controller

// envtest suite for the AgentSession state machine. It runs a real
// kube-apiserver and etcd (no kubelet, no garbage collector), calls the
// reconciler directly, and plays the kubelet's part by patching pod status.
//
// Run with KUBEBUILDER_ASSETS pointing at envtest binaries, for example:
//   KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25 use -p path) go test ./internal/controller/
// Without it the suite is skipped.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
)

var (
	k8s    client.Client
	k8sCfg *rest.Config
	scheme = k8sruntime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		os.Stdout.WriteString("envtest tests skipped: KUBEBUILDER_ASSETS not set\n")
		os.Exit(m.Run())
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{CRDs: loadCRDs()}}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	k8sCfg = cfg
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: common.Namespace}}
	if err := k8s.Create(context.Background(), ns); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// loadCRDs reads the CRD from the Helm chart, so the test always uses the
// schema that ships.
func loadCRDs() []*apiextv1.CustomResourceDefinition {
	_, file, _, _ := runtime.Caller(0)
	var out []*apiextv1.CustomResourceDefinition
	for _, path := range []string{
		filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "anton", "templates", "crd.yaml"),
		filepath.Join(filepath.Dir(file), "testdata", "volumesnapshot-crd.yaml"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		out = append(out, readCRDs(raw)...)
	}
	return out
}

func readCRDs(raw []byte) []*apiextv1.CustomResourceDefinition {
	var out []*apiextv1.CustomResourceDefinition
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		var meta metav1.TypeMeta
		if yaml.Unmarshal(doc, &meta) != nil || meta.Kind != "CustomResourceDefinition" {
			continue
		}
		crd := &apiextv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(doc, crd); err != nil {
			panic(err)
		}
		out = append(out, crd)
	}
	if len(out) == 0 {
		panic("no CRD found")
	}
	return out
}

// fakeTier records calls and serves blocks for the sessions in `has`.
type fakeTier struct {
	mu      sync.Mutex
	has     map[string]bool
	deleted []string
}

func newTier(t *testing.T) (*fakeTier, string) {
	if k8s == nil {
		t.Skip("needs envtest (KUBEBUILDER_ASSETS)")
	}
	ft := &fakeTier{has: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		name := filepath.Base(r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			if !ft.has[name] {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"tokens":1000,"bytes":4096}`))
		case http.MethodDelete:
			ft.deleted = append(ft.deleted, name)
			delete(ft.has, name)
		}
	}))
	t.Cleanup(srv.Close)
	return ft, srv.URL
}

func newReconciler(tierURL string) *SessionReconciler {
	return &SessionReconciler{Client: k8s, Scheme: scheme, Image: "anton:test", TierURL: tierURL}
}

func setMode(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: common.ModeConfigMap, Namespace: common.Namespace}}
	_ = k8s.Delete(ctx, cm)
	cm.Data = map[string]string{"mode": mode}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
}

func createSession(t *testing.T, name string, spec v1.AgentSessionSpec) {
	t.Helper()
	s := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: common.Namespace}, Spec: spec}
	if err := k8s.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, r *SessionReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: common.Namespace, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

// settle reconciles until the session stops changing (or n passes).
func settle(t *testing.T, r *SessionReconciler, name string, n int) {
	t.Helper()
	prev := ""
	for i := 0; i < n; i++ {
		reconcile(t, r, name)
		s := get(t, name)
		if s == nil {
			return
		}
		cur := s.ResourceVersion
		if cur == prev {
			return
		}
		prev = cur
	}
}

func get(t *testing.T, name string) *v1.AgentSession {
	t.Helper()
	var s v1.AgentSession
	err := k8s.Get(context.Background(), types.NamespacedName{Namespace: common.Namespace, Name: name}, &s)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

func sandbox(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	err := k8s.Get(context.Background(), types.NamespacedName{Namespace: common.Namespace, Name: name + "-sandbox"}, &p)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

// markReady plays the kubelet: binds the pod to a node and reports it Ready.
// The IP is a loopback port nobody listens on, so agent calls fail fast.
func markReady(t *testing.T, name, node string) {
	t.Helper()
	ctx := context.Background()
	p := sandbox(t, name)
	if p == nil {
		t.Fatalf("%s: no sandbox pod", name)
	}
	if p.Spec.NodeName == "" {
		// nodeName can only be set at creation, so recreate the pod bound to the node.
		bound := p.DeepCopy()
		bound.ResourceVersion, bound.UID = "", ""
		bound.Spec.NodeName = node
		if err := k8s.Delete(ctx, p, client.GracePeriodSeconds(0)); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Create(ctx, bound); err != nil {
			t.Fatal(err)
		}
		p = bound
	}
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = "127.0.0.1"
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func removePod(t *testing.T, name string) {
	t.Helper()
	if p := sandbox(t, name); p != nil {
		p.Finalizers = nil
		_ = k8s.Update(context.Background(), p)
		if err := k8s.Delete(context.Background(), p, client.GracePeriodSeconds(0)); err != nil && !errors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

func setDesired(t *testing.T, name, desired string) {
	t.Helper()
	s := get(t, name)
	orig := s.DeepCopy()
	s.Spec.DesiredState = desired
	if err := k8s.Patch(context.Background(), s, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
}

func wantPhase(t *testing.T, name, phase string) *v1.AgentSession {
	t.Helper()
	s := get(t, name)
	if s.Status.Phase != phase {
		t.Fatalf("%s: phase %q, want %q (message %q)", name, s.Status.Phase, phase, s.Status.Message)
	}
	return s
}

func TestCreateBecomesPendingThenActive(t *testing.T) {
	_, tier := newTier(t)
	r := newReconciler(tier)
	createSession(t, "life", v1.AgentSessionSpec{Team: "t"})

	settle(t, r, "life", 5)
	s := wantPhase(t, "life", v1.PhasePending)
	if len(s.Finalizers) != 1 || s.Finalizers[0] != common.Finalizer {
		t.Fatalf("finalizers %v", s.Finalizers)
	}
	p := sandbox(t, "life")
	if p == nil {
		t.Fatal("sandbox pod not created")
	}
	if ts := p.Spec.TopologySpreadConstraints; len(ts) != 1 || ts[0].TopologyKey != "kubernetes.io/hostname" {
		t.Fatalf("sandbox should spread over nodes: %+v", ts)
	}
	if p.Spec.SecurityContext == nil || p.Spec.SecurityContext.RunAsNonRoot == nil || !*p.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("sandbox must run as non-root")
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "life" || p.OwnerReferences[0].Controller == nil || !*p.OwnerReferences[0].Controller {
		t.Fatalf("sandbox owner references %+v", p.OwnerReferences)
	}

	markReady(t, "life", "node-a")
	settle(t, r, "life", 5)
	s = wantPhase(t, "life", v1.PhaseActive)
	if s.Status.Node != "node-a" || s.Status.SandboxPod != "life-sandbox" {
		t.Fatalf("status %+v", s.Status)
	}
	if s.Status.LastTransition == nil {
		t.Fatal("lastTransition not set")
	}
}

func TestLostSandboxIsMigrating(t *testing.T) {
	_, tier := newTier(t)
	r := newReconciler(tier)
	createSession(t, "mig", v1.AgentSessionSpec{})
	settle(t, r, "mig", 5)
	markReady(t, "mig", "node-a")
	settle(t, r, "mig", 5)
	wantPhase(t, "mig", v1.PhaseActive)

	removePod(t, "mig") // node drained or vanished
	settle(t, r, "mig", 5)
	wantPhase(t, "mig", v1.PhaseMigrating)
	if sandbox(t, "mig") == nil {
		t.Fatal("replacement sandbox not created")
	}
	markReady(t, "mig", "node-b")
	settle(t, r, "mig", 5)
	if s := wantPhase(t, "mig", v1.PhaseActive); s.Status.Node != "node-b" {
		t.Fatalf("node %q, want node-b", s.Status.Node)
	}
}

func TestHibernateAndWake(t *testing.T) {
	_, tier := newTier(t)
	setMode(t, common.ModePlain)
	r := newReconciler(tier)
	createSession(t, "hib", v1.AgentSessionSpec{})
	settle(t, r, "hib", 5)
	markReady(t, "hib", "node-a")
	settle(t, r, "hib", 5)

	setDesired(t, "hib", v1.StateHibernated)
	settle(t, r, "hib", 5)
	s := wantPhase(t, "hib", v1.PhaseHibernated)
	if s.Status.Hibernations != 1 || s.Status.CacheLocation != v1.CacheNone {
		t.Fatalf("status %+v", s.Status)
	}
	if s.Status.Node != "" || s.Status.SandboxPod != "" || s.Status.Replica != "" {
		t.Fatalf("placement not cleared: %+v", s.Status)
	}
	if p := sandbox(t, "hib"); p != nil && p.DeletionTimestamp == nil {
		t.Fatal("sandbox still running after hibernation")
	}

	// Reconciling a hibernated session again must not hibernate it twice.
	settle(t, r, "hib", 3)
	if s := get(t, "hib"); s.Status.Hibernations != 1 {
		t.Fatalf("hibernations %d after re-reconcile, want 1", s.Status.Hibernations)
	}

	removePod(t, "hib") // let the terminating pod go
	setDesired(t, "hib", v1.StateActive)
	settle(t, r, "hib", 5)
	wantPhase(t, "hib", v1.PhaseWaking)
	markReady(t, "hib", "node-c")
	settle(t, r, "hib", 5)
	wantPhase(t, "hib", v1.PhaseActive)
}

// Failure rule: a failed KV offload never blocks hibernation. If the blocks
// are already in the tier (a rescue ran earlier) the session records that.
func TestHibernateFailsToRecomputeOrFindsTierCopy(t *testing.T) {
	ft, tier := newTier(t)
	setMode(t, common.ModeAnton)
	r := newReconciler(tier)

	for _, tc := range []struct {
		name   string
		inTier bool
		want   string
	}{
		{"offload-fails", false, v1.CacheNone},
		{"tier-copy", true, v1.CacheTier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createSession(t, tc.name, v1.AgentSessionSpec{})
			settle(t, r, tc.name, 5)
			markReady(t, tc.name, "node-a")
			settle(t, r, tc.name, 5)

			// The session points at a replica whose server is unreachable.
			replica := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name + "-replica", Namespace: common.Namespace},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "r", Image: "x"}}},
			}
			if err := k8s.Create(context.Background(), replica); err != nil {
				t.Fatal(err)
			}
			replica.Status.PodIP = "127.0.0.1"
			if err := k8s.Status().Update(context.Background(), replica); err != nil {
				t.Fatal(err)
			}
			if err := common.PatchStatus(context.Background(), k8s, tc.name, func(s *v1.AgentSession) { s.Status.Replica = replica.Name }); err != nil {
				t.Fatal(err)
			}
			ft.mu.Lock()
			ft.has[tc.name] = tc.inTier
			ft.mu.Unlock()

			setDesired(t, tc.name, v1.StateHibernated)
			settle(t, r, tc.name, 5)
			s := wantPhase(t, tc.name, v1.PhaseHibernated)
			if s.Status.CacheLocation != tc.want {
				t.Fatalf("cache location %q, want %q (message %q)", s.Status.CacheLocation, tc.want, s.Status.Message)
			}
		})
	}
}

func TestIdleSessionAsksToHibernate(t *testing.T) {
	_, tier := newTier(t)
	r := newReconciler(tier)
	createSession(t, "idle", v1.AgentSessionSpec{IdleTimeoutSeconds: 1})
	settle(t, r, "idle", 5)
	markReady(t, "idle", "node-a")
	res := reconcile(t, r, "idle")
	if res.RequeueAfter <= 0 {
		t.Fatalf("active session with idle timeout must requeue, got %+v", res)
	}
	wantPhase(t, "idle", v1.PhaseActive)

	time.Sleep(1100 * time.Millisecond)
	reconcile(t, r, "idle")
	if s := get(t, "idle"); s.Spec.DesiredState != v1.StateHibernated {
		t.Fatalf("desiredState %q after idle timeout", s.Spec.DesiredState)
	}
}

func TestDeleteCleansTierAndRemovesFinalizer(t *testing.T) {
	ft, tier := newTier(t)
	r := newReconciler(tier)
	createSession(t, "gone", v1.AgentSessionSpec{})
	settle(t, r, "gone", 5)

	if err := k8s.Delete(context.Background(), get(t, "gone")); err != nil {
		t.Fatal(err)
	}
	if s := get(t, "gone"); s == nil || s.DeletionTimestamp.IsZero() {
		t.Fatal("finalizer did not hold the session for cleanup")
	}
	reconcile(t, r, "gone")
	if s := get(t, "gone"); s != nil {
		t.Fatalf("session still present with finalizers %v", s.Finalizers)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.deleted) != 1 || ft.deleted[0] != "gone" {
		t.Fatalf("tier deletes %v, want [gone]", ft.deleted)
	}
}

func TestWaitingPhase(t *testing.T) {
	for prev, want := range map[string]string{
		"":                  v1.PhasePending,
		v1.PhasePending:     v1.PhasePending,
		v1.PhaseHibernated:  v1.PhaseWaking,
		v1.PhaseHibernating: v1.PhaseWaking,
		v1.PhaseWaking:      v1.PhaseWaking,
		v1.PhaseRescuing:    v1.PhaseRescuing,
		v1.PhaseActive:      v1.PhaseMigrating,
		v1.PhaseMigrating:   v1.PhaseMigrating,
	} {
		if got := waitingPhase(prev); got != want {
			t.Errorf("waitingPhase(%q) = %q, want %q", prev, got, want)
		}
	}
}

// ---------- PVC workspace mode ----------

func pvcReconciler(tierURL string) *SessionReconciler {
	r := newReconciler(tierURL)
	r.Workspace, r.WorkspaceSize, r.SnapshotTimeout = WorkspacePVC, "2Gi", 2*time.Second
	return r
}

func pvc(t *testing.T, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	var p corev1.PersistentVolumeClaim
	err := k8s.Get(context.Background(), types.NamespacedName{Namespace: common.Namespace, Name: name + "-workspace"}, &p)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

// pvcGone: envtest has no pvc-protection controller, so a deleted PVC
// disappears at once or carries a deletion timestamp.
func pvcGone(t *testing.T, name string) bool {
	p := pvc(t, name)
	return p == nil || p.DeletionTimestamp != nil
}

func snapshot(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	r := &SessionReconciler{Client: k8s}
	u, err := r.getSnapshot(context.Background(), common.Namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func markSnapshotReady(t *testing.T, name string) {
	t.Helper()
	u := snapshot(t, name)
	if u == nil {
		t.Fatalf("snapshot %s not created", name)
	}
	_ = unstructured.SetNestedField(u.Object, true, "status", "readyToUse")
	if err := k8s.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestPVCWorkspaceHibernateSnapshotsAndWakeRestores(t *testing.T) {
	_, tier := newTier(t)
	setMode(t, common.ModePlain)
	r := pvcReconciler(tier)
	createSession(t, "vol", v1.AgentSessionSpec{})
	settle(t, r, "vol", 5)

	claim := pvc(t, "vol")
	if claim == nil {
		t.Fatal("workspace PVC not created")
	}
	if claim.Spec.DataSource != nil || claim.Spec.Resources.Requests.Storage().String() != "2Gi" {
		t.Fatalf("fresh PVC spec %+v", claim.Spec)
	}
	if v := sandbox(t, "vol").Spec.Volumes[0]; v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "vol-workspace" {
		t.Fatalf("sandbox volume %+v", v)
	}
	markReady(t, "vol", "node-a")
	settle(t, r, "vol", 5)

	setDesired(t, "vol", v1.StateHibernated)
	settle(t, r, "vol", 5)
	// Waiting for the snapshot: the sandbox keeps running, the volume stays.
	wantPhase(t, "vol", v1.PhaseHibernating)
	if p := sandbox(t, "vol"); p == nil || p.DeletionTimestamp != nil {
		t.Fatal("sandbox stopped before the snapshot was ready")
	}
	if snapshot(t, "vol-ws-1") == nil {
		t.Fatal("VolumeSnapshot vol-ws-1 not created")
	}

	markSnapshotReady(t, "vol-ws-1")
	settle(t, r, "vol", 5)
	s := wantPhase(t, "vol", v1.PhaseHibernated)
	if s.Status.WorkspaceSnapshot != "vol-ws-1" {
		t.Fatalf("workspaceSnapshot %q", s.Status.WorkspaceSnapshot)
	}
	if !pvcGone(t, "vol") {
		t.Fatal("workspace PVC kept after a successful snapshot")
	}

	removePod(t, "vol")
	if p := pvc(t, "vol"); p != nil { // let envtest finish deleting it
		p.Finalizers = nil
		_ = k8s.Update(context.Background(), p)
	}
	setDesired(t, "vol", v1.StateActive)
	settle(t, r, "vol", 5)
	claim = pvc(t, "vol")
	if claim == nil || claim.Spec.DataSource == nil || claim.Spec.DataSource.Name != "vol-ws-1" || claim.Spec.DataSource.Kind != "VolumeSnapshot" {
		t.Fatalf("wake did not restore the PVC from the snapshot: %+v", claim)
	}
	wantPhase(t, "vol", v1.PhaseWaking)

	// A second hibernation takes a new snapshot and drops the old one.
	markReady(t, "vol", "node-b")
	settle(t, r, "vol", 5)
	setDesired(t, "vol", v1.StateHibernated)
	settle(t, r, "vol", 5)
	markSnapshotReady(t, "vol-ws-2")
	settle(t, r, "vol", 5)
	if s := wantPhase(t, "vol", v1.PhaseHibernated); s.Status.WorkspaceSnapshot != "vol-ws-2" || s.Status.Hibernations != 2 {
		t.Fatalf("status after second hibernation %+v", s.Status)
	}
	if old := snapshot(t, "vol-ws-1"); old != nil && old.GetDeletionTimestamp() == nil {
		t.Fatal("previous snapshot not deleted")
	}
}

// Failure rule: a snapshot that never becomes ready does not block
// hibernation, and the volume is kept so no work is lost.
func TestPVCSnapshotTimeoutKeepsVolume(t *testing.T) {
	_, tier := newTier(t)
	setMode(t, common.ModePlain)
	r := pvcReconciler(tier)
	createSession(t, "slow", v1.AgentSessionSpec{})
	settle(t, r, "slow", 5)
	markReady(t, "slow", "node-a")
	settle(t, r, "slow", 5)
	setDesired(t, "slow", v1.StateHibernated)
	settle(t, r, "slow", 5)
	wantPhase(t, "slow", v1.PhaseHibernating)

	time.Sleep(2100 * time.Millisecond) // past SnapshotTimeout
	settle(t, r, "slow", 5)
	s := wantPhase(t, "slow", v1.PhaseHibernated)
	if s.Status.WorkspaceSnapshot != "" {
		t.Fatalf("recorded an unready snapshot: %q", s.Status.WorkspaceSnapshot)
	}
	if pvcGone(t, "slow") {
		t.Fatal("volume deleted although the snapshot never became ready")
	}
}

// ---------- tenant provisioning ----------

func TestTenantNamespaceIsProvisioned(t *testing.T) {
	if k8s == nil {
		t.Skip("needs envtest (KUBEBUILDER_ASSETS)")
	}
	ctx := context.Background()
	mkns := func(name string, labels, annos map[string]string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annos}}
		if err := k8s.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	mkns(TeamNamespace("core"), map[string]string{TenantLabel: "true", TeamLabel: "core"}, map[string]string{MaxSessionsAnno: "3"})
	mkns("someone-else", nil, nil)

	var restarted []string
	r := &TenantReconciler{Client: k8s, PlatformNamespace: common.Namespace, Bindings: DefaultTenantBindings,
		Known: map[string]bool{}, OnNewTenant: func(ns string) { restarted = append(restarted, ns) }}
	for _, ns := range []string{TeamNamespace("core"), "someone-else"} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	var rbs rbacv1.RoleBindingList
	if err := k8s.List(ctx, &rbs, client.InNamespace(TeamNamespace("core"))); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, rb := range rbs.Items {
		if len(rb.Subjects) != 1 || rb.Subjects[0].Namespace != common.Namespace || rb.RoleRef.Kind != "ClusterRole" {
			t.Fatalf("binding %s: %+v %+v", rb.Name, rb.RoleRef, rb.Subjects)
		}
		got[rb.Subjects[0].Name] = rb.RoleRef.Name
	}
	want := map[string]string{"anton-operator": "anton-tenant-operator", "anton-api": "anton-tenant-api",
		"anton-router": "anton-tenant-router", "anton-bench": "anton-tenant-api"}
	for sa, role := range want {
		if got[sa] != role {
			t.Fatalf("service account %s bound to %q, want %q (all: %v)", sa, got[sa], role, got)
		}
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: TeamNamespace("core"), Name: "anton-sandbox"}, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Egress) != 3 {
		t.Fatalf("network policy %+v", np.Spec)
	}
	var q corev1.ResourceQuota
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: TeamNamespace("core"), Name: "anton-team"}, &q); err != nil {
		t.Fatal(err)
	}
	if pods := q.Spec.Hard[corev1.ResourcePods]; pods.Value() != 3 {
		t.Fatalf("quota pods %v, want the team's 3", pods.String())
	}
	if len(restarted) != 1 || restarted[0] != TeamNamespace("core") {
		t.Fatalf("restart hook calls %v", restarted)
	}

	// Not a tenant: nothing created.
	var none rbacv1.RoleBindingList
	_ = k8s.List(ctx, &none, client.InNamespace("someone-else"))
	if len(none.Items) != 0 {
		t.Fatalf("provisioned a non-tenant namespace: %d bindings", len(none.Items))
	}

	// Already covered by the cache: provisioning is idempotent and no restart.
	r.Known[TeamNamespace("core")] = true
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: TeamNamespace("core")}}); err != nil {
		t.Fatal(err)
	}
	if len(restarted) != 1 {
		t.Fatalf("restart triggered for a known namespace: %v", restarted)
	}
}

// ---------- admission policies (deploy/helm/anton/templates/admission.yaml) ----------

func loadAdmissionPolicies(t *testing.T) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "anton", "templates", "admission.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("{{ .Release.Namespace }}"), []byte(common.Namespace))
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &u.Object); err != nil || len(u.Object) == 0 {
			continue
		}
		if err := k8s.Create(context.Background(), u); err != nil && !errors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

// eventually retries until the API server has loaded the policies.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	var err error
	for i := 0; i < 50; i++ {
		if err = f(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

func TestAdmissionPoliciesConfineTenantRolesAndNamespaces(t *testing.T) {
	if k8s == nil {
		t.Skip("needs envtest (KUBEBUILDER_ASSETS)")
	}
	ctx := context.Background()
	loadAdmissionPolicies(t)
	for name, labels := range map[string]map[string]string{
		"plain-ns":           nil,
		TeamNamespace("adm"): {TenantLabel: "true", TeamLabel: "adm"},
	} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}); err != nil {
			t.Fatal(err)
		}
	}
	binding := func(ns string, n int) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", n), Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "anton-tenant-operator"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "anton-operator", Namespace: common.Namespace}},
		}
	}
	n := 0
	eventually(t, "tenant role bound outside a tenant namespace must be denied", func() error {
		n++
		err := k8s.Create(ctx, binding("plain-ns", n))
		if err == nil {
			return fmt.Errorf("allowed")
		}
		if !strings.Contains(err.Error(), "can only be bound") {
			return err
		}
		return nil
	})
	if err := k8s.Create(ctx, binding(TeamNamespace("adm"), 999)); err != nil {
		t.Fatalf("binding in a tenant namespace should be allowed: %v", err)
	}

	// Impersonate the API's service account, with RBAC that lets it create
	// namespaces: the policy must still restrict which ones.
	sa := "system:serviceaccount:" + common.Namespace + ":anton-api"
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "probe-ns-create"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"create"}}}}
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "probe-ns-create"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: cr.Name},
		Subjects: []rbacv1.Subject{{Kind: "User", Name: sa}}}
	for _, o := range []client.Object{cr, crb} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	cfg := rest.CopyConfig(k8sCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: sa}
	api, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the API must not create arbitrary namespaces", func() error {
		err := api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-evil"}})
		if err == nil {
			return fmt.Errorf("allowed")
		}
		if !strings.Contains(err.Error(), "may only create anton-team-*") {
			return err
		}
		return nil
	})
	if err := api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: TeamNamespace("newteam"),
		Labels: map[string]string{TenantLabel: "true", TeamLabel: "newteam"}}}); err != nil {
		t.Fatalf("the API should be able to create a labelled team namespace: %v", err)
	}
	if err := api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: TeamNamespace("nolabel")}}); err == nil {
		t.Fatal("an unlabelled team namespace must be refused")
	}
}
