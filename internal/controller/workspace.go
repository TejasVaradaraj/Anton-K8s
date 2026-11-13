package controller

// Workspace volumes (PVC mode). Each session's workspace is a PVC owned by
// the session. Hibernation takes a VolumeSnapshot, then deletes the pod and
// the PVC, so an idle session holds no volume; waking creates a fresh PVC
// from the snapshot. VolumeSnapshots are handled as unstructured objects to
// avoid a dependency on the external-snapshotter client.
//
// PVC mode needs a CSI driver with snapshot support. For sandboxes to follow
// a drain to another node the volume must be network-attached (EBS, PD); a
// node-local driver such as csi-hostpath pins the session to one node.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
)

const (
	WorkspaceEmptyDir = "emptyDir"
	WorkspacePVC      = "pvc"
)

var snapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

func pvcName(s *v1.AgentSession) string { return s.Name + "-workspace" }

// snapshotName is unique per hibernation, so a retried hibernation finds the
// snapshot it already started instead of taking a second one.
func snapshotName(s *v1.AgentSession) string {
	return fmt.Sprintf("%s-ws-%d", s.Name, s.Status.Hibernations+1)
}

func (r *SessionReconciler) pvcMode() bool { return r.Workspace == WorkspacePVC }

// ensureWorkspacePVC creates the session's PVC if missing, restoring it from
// the last workspace snapshot when there is one.
func (r *SessionReconciler) ensureWorkspacePVC(ctx context.Context, s *v1.AgentSession) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName(s)}, &existing)
	if err == nil || !errors.IsNotFound(err) {
		return err
	}
	size := r.WorkspaceSize
	if size == "" {
		size = "1Gi"
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName(s), Namespace: s.Namespace,
			Labels: map[string]string{"app": "anton-workspace", "anton.dev/session": s.Name}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}
	if r.StorageClass != "" {
		pvc.Spec.StorageClassName = &r.StorageClass
	}
	if snap := s.Status.WorkspaceSnapshot; snap != "" {
		group := snapshotGVK.Group
		pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: snapshotGVK.Kind, Name: snap}
	}
	if err := controllerutil.SetControllerReference(s, pvc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *SessionReconciler) getSnapshot(ctx context.Context, ns, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(snapshotGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	return u, err
}

func (r *SessionReconciler) createSnapshot(ctx context.Context, s *v1.AgentSession) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(snapshotGVK)
	u.SetNamespace(s.Namespace)
	u.SetName(snapshotName(s))
	u.SetLabels(map[string]string{"anton.dev/session": s.Name})
	spec := map[string]any{"source": map[string]any{"persistentVolumeClaimName": pvcName(s)}}
	if r.SnapshotClass != "" {
		spec["volumeSnapshotClassName"] = r.SnapshotClass
	}
	u.Object["spec"] = spec
	if err := controllerutil.SetControllerReference(s, u, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, u); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func snapshotReady(u *unstructured.Unstructured) bool {
	ready, _, _ := unstructured.NestedBool(u.Object, "status", "readyToUse")
	return ready
}

func (r *SessionReconciler) deleteSnapshot(ctx context.Context, ns, name string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(snapshotGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	if err := r.Delete(ctx, u); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *SessionReconciler) deleteWorkspacePVC(ctx context.Context, s *v1.AgentSession) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: pvcName(s)}}
	if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *SessionReconciler) workspaceVolume(s *v1.AgentSession) corev1.Volume {
	if r.pvcMode() {
		return corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(s)}}}
	}
	return corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}
