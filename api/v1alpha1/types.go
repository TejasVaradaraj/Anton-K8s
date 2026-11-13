// Package v1alpha1 defines the AgentSession custom resource.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Session phases (the state machine).
const (
	PhasePending     = "Pending"
	PhaseActive      = "Active"
	PhaseHibernating = "Hibernating"
	PhaseHibernated  = "Hibernated"
	PhaseWaking      = "Waking"
	PhaseMigrating   = "Migrating"
	PhaseRescuing    = "Rescuing"
	PhaseFailed      = "Failed"
)

// Desired states a user (or the idle detector) can ask for.
const (
	StateActive     = "Active"
	StateHibernated = "Hibernated"
)

// Where the session's KV cache currently lives.
const (
	CacheReplica = "replica"
	CacheTier    = "tier"
	CacheNone    = "none"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "anton.dev", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &AgentSession{}, &AgentSessionList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

type AgentSessionSpec struct {
	Team               string `json:"team,omitempty"`
	Model              string `json:"model,omitempty"`
	Priority           int32  `json:"priority,omitempty"`
	IdleTimeoutSeconds int32  `json:"idleTimeoutSeconds,omitempty"`
	DesiredState       string `json:"desiredState,omitempty"`
}

type AgentSessionStatus struct {
	Phase          string       `json:"phase,omitempty"`
	SandboxPod     string       `json:"sandboxPod,omitempty"`
	Node           string       `json:"node,omitempty"`
	Replica        string       `json:"replica,omitempty"`
	CacheLocation  string       `json:"cacheLocation,omitempty"`
	CachedTokens   int64        `json:"cachedTokens,omitempty"`
	LastActivity   *metav1.Time `json:"lastActivity,omitempty"`
	LastTransition *metav1.Time `json:"lastTransition,omitempty"`
	Message        string       `json:"message,omitempty"`
	Hibernations   int32        `json:"hibernations,omitempty"`
	// WorkspaceSnapshot names the VolumeSnapshot a hibernated session's
	// workspace volume is restored from on wake (PVC workspace mode only).
	WorkspaceSnapshot string `json:"workspaceSnapshot,omitempty"`
	Rescues           int32  `json:"rescues,omitempty"`
}

type AgentSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSessionSpec   `json:"spec,omitempty"`
	Status            AgentSessionStatus `json:"status,omitempty"`
}

type AgentSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSession `json:"items"`
}

func (in *AgentSession) DeepCopyInto(out *AgentSession) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Status.LastActivity != nil {
		out.Status.LastActivity = in.Status.LastActivity.DeepCopy()
	}
	if in.Status.LastTransition != nil {
		out.Status.LastTransition = in.Status.LastTransition.DeepCopy()
	}
}

func (in *AgentSession) DeepCopy() *AgentSession {
	if in == nil {
		return nil
	}
	out := new(AgentSession)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentSession) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *AgentSessionList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(AgentSessionList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]AgentSession, len(in.Items))
	for i := range in.Items {
		in.Items[i].DeepCopyInto(&out.Items[i])
	}
	return out
}
