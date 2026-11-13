// The Anton operator: the AgentSession reconciler plus the disruption guard.
package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	tenancy := flag.Bool("tenancy", false, "one namespace per team (anton-team-<team>): provision them and reconcile sessions there")
	flag.Parse()
	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	cfg := ctrl.GetConfigOrDie()
	// The cache covers Anton's namespace and, with tenancy on, the team
	// namespaces that exist now: RBAC grants pods and PVCs there and nowhere
	// else (nodes stay cluster-scoped), and a controller-runtime cache cannot
	// take a namespace it may not list. A team created later restarts the
	// operator (see below), which rebuilds the cache with it.
	namespaces := map[string]cache.Config{common.Namespace: {}}
	known := map[string]bool{common.Namespace: true}
	// The tenant controller reads and writes through the API server directly:
	// it works in namespaces the cache does not cover yet (a new team's).
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		ctrl.Log.Error(err, "client")
		os.Exit(1)
	}
	if *tenancy {
		tenants, err := common.TenantNamespaces(context.Background(), direct)
		if err != nil {
			ctrl.Log.Error(err, "listing team namespaces")
			os.Exit(1)
		}
		for _, ns := range tenants {
			// Only namespaces already provisioned: before its RoleBindings
			// exist, an informer there fails and blocks the whole cache.
			var rb rbacv1.RoleBinding
			if direct.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "anton-tenant-operator-anton-operator"}, &rb) == nil {
				namespaces[ns] = cache.Config{}
				known[ns] = true
			}
		}
		ctrl.Log.Info("tenancy on", "namespaces", len(namespaces))
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: namespaces},
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
	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	if *tenancy {
		if err := (&controller.TenantReconciler{
			Client: direct, PlatformNamespace: common.Namespace, Bindings: controller.DefaultTenantBindings, Known: known,
			OnNewTenant: func(ns string) {
				// Exit cleanly; the Deployment restarts the pod and the new
				// process builds its cache with this namespace. Sessions keep
				// running meanwhile: the data plane does not need the operator.
				ctrl.Log.Info("new team namespace provisioned: restarting to watch it", "namespace", ns)
				cancel()
			},
		}).SetupWithManager(mgr); err != nil {
			ctrl.Log.Error(err, "tenant controller")
			os.Exit(1)
		}
	}
	if err := mgr.Add(&controller.Guard{Client: mgr.GetClient(), PreStop: *preStop, HalfLife: *halfLife}); err != nil {
		ctrl.Log.Error(err, "guard")
		os.Exit(1)
	}
	ctrlmetrics.Registry.MustRegister(metrics.SessionCollector{Reader: mgr.GetCache()})
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err := mgr.Start(ctx); err != nil {
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
