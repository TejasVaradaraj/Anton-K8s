package controller

// The tenant controller provisions team namespaces (docs/multitenancy.md).
//
// The API creates a namespace anton-team-<team> labelled anton.dev/tenant=true
// when a team is created. This controller then gives the platform's service
// accounts access to that namespace and nothing else (RoleBindings to the
// anton-tenant-* ClusterRoles, which are never bound cluster-wide), isolates
// the team's sandboxes (default-deny NetworkPolicy: they may reach only the
// router, the cache tier and DNS in the platform namespace) and applies the
// team's quota.
//
// The operator's cache only covers namespaces it knew at startup (its RBAC
// does not allow cluster-wide pod access). When a tenant namespace appears
// that the cache does not cover, OnNewTenant is called once provisioning is
// done; the operator uses it to restart its manager with the new namespace
// set (a leader hand-off of a few seconds; teams are created rarely).

import (
	"context"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	TenantLabel        = "anton.dev/tenant"
	TeamLabel          = "anton.dev/team"
	MaxSessionsAnno    = "anton.dev/max-sessions"
	TenantNamespacePre = "anton-team-"
)

// TeamNamespace is the namespace that holds a team's sessions.
func TeamNamespace(team string) string { return TenantNamespacePre + team }

// TenantBinding says which platform service account gets which tenant role.
type TenantBinding struct {
	ClusterRole    string
	ServiceAccount string
}

type TenantReconciler struct {
	client.Client
	PlatformNamespace string
	Bindings          []TenantBinding
	// Known is the set of namespaces the operator's cache covers.
	Known map[string]bool
	// OnNewTenant runs after a namespace outside Known is provisioned.
	OnNewTenant func(namespace string)
}

// DefaultTenantBindings are the platform service accounts and their roles.
var DefaultTenantBindings = []TenantBinding{
	{ClusterRole: "anton-tenant-operator", ServiceAccount: "anton-operator"},
	{ClusterRole: "anton-tenant-api", ServiceAccount: "anton-api"},
	{ClusterRole: "anton-tenant-router", ServiceAccount: "anton-router"},
	{ClusterRole: "anton-tenant-api", ServiceAccount: "anton-bench"},
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	isTenant := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetLabels()[TenantLabel] == "true" })
	return ctrl.NewControllerManagedBy(mgr).
		Named("tenant").
		For(&corev1.Namespace{}, builder.WithPredicates(isTenant)).
		Complete(r)
}

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ns.Labels[TenantLabel] != "true" || !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	name := ns.Name
	for _, b := range r.Bindings {
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: b.ClusterRole + "-" + b.ServiceAccount, Namespace: name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
			rb.Labels = map[string]string{"app.kubernetes.io/managed-by": "anton-operator"}
			// roleRef is immutable; CreateOrUpdate only sets it on create.
			rb.RoleRef = rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: b.ClusterRole}
			rb.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: b.ServiceAccount, Namespace: r.PlatformNamespace}}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.ensureNetworkPolicy(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureQuota(ctx, &ns); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureLimitRange(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	if !r.Known[name] && r.OnNewTenant != nil {
		r.OnNewTenant(name)
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) ensureNetworkPolicy(ctx context.Context, ns string) error {
	platform := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": r.PlatformNamespace}}
	port := func(p int) *intstr.IntOrString { v := intstr.FromInt32(int32(p)); return &v }
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	from := func(app string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{NamespaceSelector: platform,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}}
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "anton-sandbox", Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec = networkingv1.NetworkPolicySpec{
			// Every pod in a team namespace is a sandbox: deny by default.
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{from("anton-operator"), from("anton-api"), from("anton-bench")},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: port(8080)}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{To: []networkingv1.NetworkPolicyPeer{from("anton-tier")}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: port(9000)}}},
				{To: []networkingv1.NetworkPolicyPeer{from("anton-router")}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: port(8080)}}},
				{To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: port(53)}, {Protocol: &tcp, Port: port(53)}}},
			},
		}
		return nil
	})
	return err
}

// ensureQuota caps what a team namespace can hold. The API enforces session
// quotas too; this is the backstop if anything bypasses the API.
func (r *TenantReconciler) ensureQuota(ctx context.Context, ns *corev1.Namespace) error {
	max := 10
	if v, err := strconv.Atoi(ns.Annotations[MaxSessionsAnno]); err == nil && v > 0 {
		max = v
	}
	q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "anton-team", Namespace: ns.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, q, func() error {
		q.Spec.Hard = corev1.ResourceList{
			corev1.ResourcePods:                             resource.MustParse(strconv.Itoa(max)),
			corev1.ResourcePersistentVolumeClaims:           resource.MustParse(strconv.Itoa(max)),
			corev1.ResourceRequestsStorage:                  resource.MustParse(strconv.Itoa(max*2) + "Gi"),
			"count/volumesnapshots.snapshot.storage.k8s.io": resource.MustParse(strconv.Itoa(max * 2)),
			"count/agentsessions.anton.dev":                 resource.MustParse(strconv.Itoa(max)),
		}
		return nil
	})
	return err
}

func (r *TenantReconciler) ensureLimitRange(ctx context.Context, ns string) error {
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "anton-sandbox", Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, lr, func() error {
		lr.Spec.Limits = []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		}}
		return nil
	})
	return err
}
