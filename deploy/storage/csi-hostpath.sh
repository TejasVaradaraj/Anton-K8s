#!/usr/bin/env bash
# VolumeSnapshot support on kind: the snapshot CRDs and controller from
# external-snapshotter, and the CSI hostpath driver with a StorageClass
# (csi-hostpath-sc) and a VolumeSnapshotClass (csi-hostpath-snapclass).
#
# The hostpath driver keeps volumes on the node running its plugin, so every
# PVC-backed sandbox lands on that node. It demonstrates snapshot-based
# hibernation; cross-node migration needs network storage (EBS, PD).
set -euo pipefail
SNAPSHOTTER=${SNAPSHOTTER:-v8.2.0}
HOSTPATH=${HOSTPATH:-v1.15.0}

kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=$SNAPSHOTTER"
kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=$SNAPSHOTTER"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git clone -q --depth 1 -b "$HOSTPATH" https://github.com/kubernetes-csi/csi-driver-host-path "$work/hostpath"
"$work/hostpath/deploy/kubernetes-latest/deploy.sh"
kubectl apply -f "$work/hostpath/examples/csi-storageclass.yaml"
kubectl apply -f "$work/hostpath/examples/csi-volumesnapshotclass.yaml"
