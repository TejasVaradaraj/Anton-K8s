// The Anton operator: the AgentSession reconciler plus the disruption guard.
package main

import (
	"strings"

	"flag"
	corev1 "k8s.io/api/core/v1"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/controller"
	"github.com/TejasVaradaraj/anton-platform/internal/metrics"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func main() {
	image := flag.String("image", "anton-sandbox:dev", "image used for sandbox pods")
	tier := flag.String("tier-url", "http://anton-tier:9000", "cache tier base URL")
	preStop := flag.Duration("prestop", 20*time.Second, "preStop window of inference replicas")
	halfLife := flag.Duration("half-life", 5*time.Minute, "reuse half-life used in the cache-value score")
	workspace := flag.String("workspace", "emptyDir", "sandbox workspace storage: emptyDir or pvc")
	storageClass := flag.String("storage-class", "", "StorageClass for workspace PVCs (empty = cluster default)")
	snapshotClass := flag.String("snapshot-class", "", "VolumeSnapshotClass for workspace snapshots (empty = default)")
	workspaceSize := flag.String("workspace-size", "1Gi", "size of each workspace PVC")
	agent := flag.String("agent", "builtin", "agent inside each sandbox: builtin or opencode")
	model := flag.String("model", "anton-mock", "model name agents request from the router")
	routerURL := flag.String("router-url", "http://anton-router:8080", "session router base URL, as sandboxes reach it")
	tolerate := flag.String("sandbox-tolerate", "", "comma-separated taint keys sandboxes tolerate (e.g. nvidia.com/gpu)")
	runtimeClass := flag.String("runtime-class", "", "RuntimeClass for sandbox pods, e.g. gvisor (empty = default)")
	leaderElect := flag.Bool("leader-elect", false, "run several replicas; only the elected leader reconciles and guards")
	flag.Parse()
	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Watch only Anton's namespace: its RBAC grants pods, PVCs and
		// ConfigMaps there and nowhere else (nodes stay cluster-scoped).
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{common.Namespace: {}}},
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
		// Standbys wait on a Lease and take over within ~15 s if the leader
		// dies. The guard is a Runnable that needs leadership too (the
		// default), so two guards never plan rescues for the same replica.
		LeaderElection:                *leaderElect,
		LeaderElectionID:              "anton-operator.anton.dev",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		ctrl.Log.Error(err, "manager")
		os.Exit(1)
	}
	if err := (&controller.SessionReconciler{
		Client: mgr.GetClient(), Scheme: scheme, Image: *image, TierURL: *tier,
		Workspace: *workspace, StorageClass: *storageClass, SnapshotClass: *snapshotClass, WorkspaceSize: *workspaceSize,
		RuntimeClass: *runtimeClass, Agent: *agent, Model: *model, RouterURL: *routerURL,
		SandboxTolerations: tolerations(*tolerate),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "controller")
		os.Exit(1)
	}
	if err := mgr.Add(&controller.Guard{Client: mgr.GetClient(), PreStop: *preStop, HalfLife: *halfLife}); err != nil {
		ctrl.Log.Error(err, "guard")
		os.Exit(1)
	}
	ctrlmetrics.Registry.MustRegister(metrics.SessionCollector{Reader: mgr.GetCache()})
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "run")
		os.Exit(1)
	}
}

// tolerations turns "a,b" into Exists tolerations for those taint keys.
func tolerations(keys string) []corev1.Toleration {
	var out []corev1.Toleration
	for _, k := range strings.Split(keys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, corev1.Toleration{Key: k, Operator: corev1.TolerationOpExists})
		}
	}
	return out
}
