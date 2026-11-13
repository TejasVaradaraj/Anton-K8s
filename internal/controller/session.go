// Package controller holds the AgentSession reconciler and the disruption guard.
package controller

import (
	"context"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/metrics"
)

// SessionReconciler drives one AgentSession toward its desired state:
// Active means a ready sandbox pod exists, Hibernated means the workspace is
// snapshotted, the KV cache is in the tier, and no pod is running.
type SessionReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Image   string
	TierURL string

	// Workspace storage: WorkspaceEmptyDir (default) or WorkspacePVC.
	Workspace     string
	StorageClass  string
	SnapshotClass string
	WorkspaceSize string
	// RuntimeClass for sandbox pods (for example "gvisor"); empty = default runtime.
	RuntimeClass string
	// Agent inside the sandbox: "builtin" or "opencode"; Model is the model
	// name it asks the router for; RouterURL is the session router.
	Agent, Model, RouterURL string
	// SandboxTolerations let sandboxes share nodes with tainted inference
	// replicas (e.g. GPU nodes), so a node loss hits both, as in production.
	SandboxTolerations []corev1.Toleration
	// How long hibernation waits for a workspace snapshot before giving up
	// on it (the volume is then kept instead of deleted). Default 2 minutes.
	SnapshotTimeout time.Duration
}

func (r *SessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.AgentSession{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: 8}).
		Complete(r)
}

func (r *SessionReconciler) setStatus(ctx context.Context, s *v1.AgentSession, f func(*v1.AgentSessionStatus)) error {
	orig := s.DeepCopy()
	prev := s.Status.Phase
	f(&s.Status)
	if s.Status.Phase != prev {
		now := metav1.Now()
		s.Status.LastTransition = &now
	}
	return r.Status().Patch(ctx, s, client.MergeFrom(orig))
}

func (r *SessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != common.Namespace {
		return ctrl.Result{}, nil
	}
	var s v1.AgentSession
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: the finalizer gives us a chance to clean the cache tier. The
	// sandbox pod is removed by its owner reference.
	if !s.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&s, common.Finalizer) {
			_ = kv.Do(ctx, http.MethodDelete, r.TierURL+"/blocks/"+s.Name, nil, nil)
			controllerutil.RemoveFinalizer(&s, common.Finalizer)
			return ctrl.Result{}, r.Update(ctx, &s)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&s, common.Finalizer) {
		return ctrl.Result{}, r.Update(ctx, &s)
	}

	desired := s.Spec.DesiredState
	if desired == "" {
		desired = v1.StateActive
	}

	// Idle detection: an Active session untouched for idleTimeoutSeconds is
	// asked to hibernate. A fresh wake counts as activity so it cannot flap.
	idle := time.Duration(s.Spec.IdleTimeoutSeconds) * time.Second
	var idleLeft time.Duration
	if desired == v1.StateActive && s.Status.Phase == v1.PhaseActive && idle > 0 {
		last := s.CreationTimestamp.Time
		for _, t := range []*metav1.Time{s.Status.LastActivity, s.Status.LastTransition} {
			if t != nil && t.After(last) {
				last = t.Time
			}
		}
		if idleLeft = idle - time.Since(last); idleLeft <= 0 {
			orig := s.DeepCopy()
			s.Spec.DesiredState = v1.StateHibernated
			return ctrl.Result{}, r.Patch(ctx, &s, client.MergeFrom(orig))
		}
	}

	var pod *corev1.Pod
	var found corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name + "-sandbox"}, &found)
	if err == nil {
		pod = &found
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if desired == v1.StateHibernated {
		return r.hibernate(ctx, &s, pod)
	}

	// desired == Active
	if r.pvcMode() {
		if err := r.ensureWorkspacePVC(ctx, &s); err != nil {
			return ctrl.Result{}, err
		}
	}
	if pod == nil {
		p := r.sandboxPod(&s, common.PersistsWorkspace(common.Mode(ctx, r.Client)))
		if err := controllerutil.SetControllerReference(&s, p, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, p); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, r.setStatus(ctx, &s, func(st *v1.AgentSessionStatus) {
			st.Phase = waitingPhase(st.Phase)
			st.SandboxPod = p.Name
		})
	}
	if !common.PodReady(pod) {
		if next := waitingPhase(s.Status.Phase); next != s.Status.Phase {
			if err := r.setStatus(ctx, &s, func(st *v1.AgentSessionStatus) { st.Phase = next }); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if s.Status.Phase != v1.PhaseRescuing && (s.Status.Phase != v1.PhaseActive || s.Status.Node != pod.Spec.NodeName) {
		if err := r.setStatus(ctx, &s, func(st *v1.AgentSessionStatus) {
			st.Phase = v1.PhaseActive
			st.Node = pod.Spec.NodeName
			st.SandboxPod = pod.Name
		}); err != nil {
			return ctrl.Result{}, err
		}
		idleLeft = idle
	}
	if idle > 0 {
		return ctrl.Result{RequeueAfter: max(idleLeft, 0) + time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// waitingPhase names what a session without a ready sandbox is doing, based
// on where it came from. Rescuing is owned by the guard and left alone.
func waitingPhase(prev string) string {
	switch prev {
	case "", v1.PhasePending:
		return v1.PhasePending
	case v1.PhaseHibernated, v1.PhaseHibernating, v1.PhaseWaking:
		return v1.PhaseWaking
	case v1.PhaseRescuing:
		return v1.PhaseRescuing
	default:
		return v1.PhaseMigrating
	}
}

// hibernate moves a session to Hibernated: snapshot the workspace, move the
// KV cache to the tier, stop the sandbox. It can take several passes (a
// VolumeSnapshot needs time to become ready), so each step is safe to repeat
// and the expensive ones run once per hibernation.
func (r *SessionReconciler) hibernate(ctx context.Context, s *v1.AgentSession, pod *corev1.Pod) (ctrl.Result, error) {
	if s.Status.Phase == v1.PhaseHibernated {
		// Already hibernated. The pod may still be terminating (it snapshots on
		// SIGTERM); never redo the offload or touch the recorded cache location.
		if pod != nil && pod.DeletionTimestamp == nil {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if s.Status.Phase != v1.PhaseHibernating {
		if err := r.setStatus(ctx, s, func(st *v1.AgentSessionStatus) { st.Phase = v1.PhaseHibernating }); err != nil {
			return ctrl.Result{}, err
		}
	}

	var snap *unstructured.Unstructured
	if r.pvcMode() {
		var err error
		if snap, err = r.getSnapshot(ctx, s.Namespace, snapshotName(s)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if snap == nil {
		// First pass of this hibernation: flush the workspace and move the KV cache.
		loc, msg := r.flushAndOffload(ctx, s, pod)
		if err := r.setStatus(ctx, s, func(st *v1.AgentSessionStatus) { st.CacheLocation, st.Message = loc, msg }); err != nil {
			return ctrl.Result{}, err
		}
		if r.pvcMode() {
			if err := r.createSnapshot(ctx, s); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	// PVC mode: wait (bounded) for the snapshot. Failure rule: a snapshot that
	// never becomes ready does not block hibernation; the volume is kept.
	snapshotOK := false
	if r.pvcMode() {
		timeout := r.SnapshotTimeout
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
		started := snap.GetCreationTimestamp().Time
		if !snapshotReady(snap) && time.Since(started) < timeout {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		snapshotOK = snapshotReady(snap)
	}

	// Stop the sandbox.
	if pod != nil {
		if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	msg := s.Status.Message
	prevSnap := s.Status.WorkspaceSnapshot
	if r.pvcMode() {
		if snapshotOK {
			// pvc-protection keeps the volume until the terminating pod is gone.
			if err := r.deleteWorkspacePVC(ctx, s); err != nil {
				return ctrl.Result{}, err
			}
			msg += "; workspace snapshot " + snap.GetName()
		} else {
			msg += "; workspace snapshot not ready in time, keeping the volume"
		}
	}
	if s.Status.LastTransition != nil {
		metrics.HibernateSeconds.Observe(time.Since(s.Status.LastTransition.Time).Seconds())
	}
	metrics.Hibernations.WithLabelValues(s.Status.CacheLocation).Inc()
	if err := r.setStatus(ctx, s, func(st *v1.AgentSessionStatus) {
		st.Phase = v1.PhaseHibernated
		st.Replica, st.SandboxPod, st.Node = "", "", ""
		st.Hibernations++
		st.Message = msg
		if snapshotOK {
			st.WorkspaceSnapshot = snap.GetName()
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if snapshotOK && prevSnap != "" && prevSnap != snap.GetName() {
		if err := r.deleteSnapshot(ctx, s.Namespace, prevSnap); err != nil {
			log.FromContext(ctx).Info("could not delete old workspace snapshot", "snapshot", prevSnap, "err", err.Error())
		}
	}
	return ctrl.Result{}, nil
}

// flushAndOffload asks the agent to save its workspace and moves the
// session's KV cache to the tier. Neither step can fail hibernation: the rule
// is "fail to recompute". Returns the cache location and a status message.
func (r *SessionReconciler) flushAndOffload(ctx context.Context, s *v1.AgentSession, pod *corev1.Pod) (string, string) {
	l := log.FromContext(ctx)
	if pod != nil && pod.Status.PodIP != "" && pod.DeletionTimestamp == nil {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := kv.Do(c, http.MethodPost, common.AgentURL(pod.Status.PodIP)+"/snapshot", nil, nil); err != nil {
			l.Info("workspace snapshot failed; relying on the SIGTERM snapshot", "err", err.Error())
		}
		cancel()
	}
	loc, msg := v1.CacheNone, "hibernated"
	if common.Mode(ctx, r.Client) == common.ModeAnton && s.Status.Replica != "" {
		var rp corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Status.Replica}, &rp); err == nil && rp.Status.PodIP != "" {
			url := common.ReplicaURL(rp.Status.PodIP)
			move, why := true, ""
			ic, icancel := context.WithTimeout(ctx, 5*time.Second)
			if info, err := kv.Sessions(ic, url); err == nil {
				move, why = hibernateMove(info, s.Name)
			}
			icancel()
			if !move {
				msg = "hibernated; " + why
			} else {
				c, cancel := context.WithTimeout(ctx, 60*time.Second)
				if err := kv.Offload(c, url, s.Name, true); err == nil {
					loc, msg = v1.CacheTier, "hibernated; KV cache offloaded to tier"
				} else {
					msg = "hibernated; KV offload from " + rp.Name + " failed, will recompute: " + err.Error()
				}
				cancel()
			}
		} else {
			msg = "hibernated; replica " + s.Status.Replica + " is gone"
		}
	} else if s.Status.Replica == "" {
		msg = "hibernated; no replica recorded for this session"
	}
	// If this pass is a retry, or a rescue already ran, the blocks may be in
	// the tier even though the offload above found nothing to move.
	if loc == v1.CacheNone && common.Mode(ctx, r.Client) == common.ModeAnton {
		if kv.Do(ctx, http.MethodGet, r.TierURL+"/blocks/"+s.Name, nil, &kv.Block{}) == nil {
			loc, msg = v1.CacheTier, msg+"; found existing blocks in tier"
		}
	}
	return loc, msg
}

// sandboxPod builds the sandbox. persist=false (the plain and routing
// baselines) makes the agent behave like an ordinary pod: its workspace dies
// with it.
func (r *SessionReconciler) sandboxPod(s *v1.AgentSession, persist bool) *corev1.Pod {
	persistEnv := "1"
	if !persist {
		persistEnv = "0"
	}
	grace := int64(15)
	uid, yes, no := int64(1000), true, false
	var runtimeClass *string
	if r.RuntimeClass != "" {
		runtimeClass = &r.RuntimeClass // e.g. gvisor: the agent runs model-generated code
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name + "-sandbox", Namespace: s.Namespace,
			Labels: map[string]string{"app": common.SandboxApp, common.SessionLabel: s.Name},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &grace,
			RuntimeClassName:              runtimeClass,
			Tolerations:                   r.SandboxTolerations,
			// Spread sandboxes over nodes so one node failure takes out as few
			// agents as possible (ScheduleAnyway: never block a sandbox on it).
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": common.SandboxApp}},
			}},
			AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, RunAsNonRoot: &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "agent", Image: r.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/bin/sim", "agent"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Env: []corev1.EnvVar{
					{Name: "SESSION_ID", Value: s.Name},
					{Name: "TIER_URL", Value: r.TierURL},
					{Name: "PERSIST_WORKSPACE", Value: persistEnv},
					{Name: "ROUTER_URL", Value: r.RouterURL},
					{Name: "AGENT", Value: r.Agent},
					{Name: "MODEL", Value: r.Model},
				},
				Ports:        []corev1.ContainerPort{{ContainerPort: common.AgentPort}},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(common.AgentPort)}},
					PeriodSeconds: 1,
				},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"),
				}},
			}},
			Volumes: []corev1.Volume{r.workspaceVolume(s)},
		},
	}
}
