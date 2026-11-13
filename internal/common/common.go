// Package common holds names and helpers shared by the operator, router and bench.
package common

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
)

const (
	Namespace      = "anton"
	ReplicaApp     = "anton-replica"
	SandboxApp     = "anton-sandbox"
	SessionLabel   = "anton.dev/session"
	SpotAnnotation = "anton.dev/spot-deadline" // RFC3339 time the node will vanish
	DeletionCost   = "controller.kubernetes.io/pod-deletion-cost"
	Finalizer      = "anton.dev/session"
	ModeConfigMap  = "anton-mode"
	ReplicaPort    = 8000
	AgentPort      = 8080

	// Modes let one deployment act as each benchmark baseline.
	ModePlain   = "plain"   // plain Kubernetes: random routing, sandboxes lose their workspace with their pod
	ModeSandbox = "sandbox" // Agent Sandbox style: workspaces persist, LLM context is not managed
	ModeRouting = "routing" // llm-d style cache-affinity routing only; sandboxes are plain pods
	ModeAnton   = "anton"   // everything: affinity routing, workspace persistence, KV rescue/offload/restore
)

// RandomRouting: the mode spreads requests like a plain Service.
func RandomRouting(mode string) bool { return mode == ModePlain || mode == ModeSandbox }

// PersistsWorkspace: sandbox workspaces survive pod loss and hibernation.
func PersistsWorkspace(mode string) bool { return mode == ModeSandbox || mode == ModeAnton }

// KVAware: the platform rescues, offloads and restores KV caches.
func KVAware(mode string) bool { return mode == ModeAnton }

// Mode reads the current mode from the anton-mode ConfigMap (default: anton).
func Mode(ctx context.Context, c client.Reader) string {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: ModeConfigMap}, &cm); err != nil {
		return ModeAnton
	}
	if m := cm.Data["mode"]; m != "" {
		return m
	}
	return ModeAnton
}

func ReplicaURL(ip string) string { return fmt.Sprintf("http://%s:%d", ip, ReplicaPort) }
func AgentURL(ip string) string   { return fmt.Sprintf("http://%s:%d", ip, AgentPort) }

// Multi-tenancy (docs/multitenancy.md): with tenancy on, a team's sessions,
// sandboxes and workspaces live in the namespace anton-team-<team>; replicas,
// the router, the tier and the operator stay in the platform namespace.
const (
	TenantLabel        = "anton.dev/tenant"
	TeamLabel          = "anton.dev/team"
	TenantNamespacePre = "anton-team-"
)

// TeamNamespace is the namespace that holds a team's sessions.
func TeamNamespace(team string) string { return TenantNamespacePre + team }

// SessionNamespace reports whether sessions may live in ns.
func SessionNamespace(ns string) bool {
	return ns == Namespace || strings.HasPrefix(ns, TenantNamespacePre)
}

// TenantNamespaces lists the provisioned team namespaces. It needs a client
// that reads the API server directly: caches are built from this list.
func TenantNamespaces(ctx context.Context, c client.Reader) ([]string, error) {
	var l corev1.NamespaceList
	if err := c.List(ctx, &l, client.MatchingLabels{TenantLabel: "true"}); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range l.Items {
		if n.DeletionTimestamp.IsZero() && strings.HasPrefix(n.Name, TenantNamespacePre) {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// FindSession looks a session up by name: in the platform namespace first,
// then in every namespace the reader's cache covers. Session names carry
// their team and are unique across the cluster.
func FindSession(ctx context.Context, c client.Reader, name string) (*v1.AgentSession, error) {
	var s v1.AgentSession
	err := c.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: name}, &s)
	if err == nil {
		return &s, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	var l v1.AgentSessionList
	if lerr := c.List(ctx, &l); lerr != nil {
		return nil, lerr
	}
	for i := range l.Items {
		if l.Items[i].Name == name {
			return &l.Items[i], nil
		}
	}
	return nil, err
}

// PatchStatus applies f to a fresh copy of the session and sends only the
// changed status fields, so the operator and router never clobber each other.
func PatchStatus(ctx context.Context, c client.Client, name string, f func(*v1.AgentSession)) error {
	found, err := FindSession(ctx, c, name)
	if err != nil {
		return err
	}
	s := *found.DeepCopy()
	orig := s.DeepCopy()
	f(&s)
	return c.Status().Patch(ctx, &s, client.MergeFrom(orig))
}

// SpotDeadline reports whether a node carries a spot-interruption notice.
func SpotDeadline(n *corev1.Node) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, n.Annotations[SpotAnnotation])
	return t, err == nil
}

func PodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
