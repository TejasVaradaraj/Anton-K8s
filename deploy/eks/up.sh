#!/usr/bin/env bash
# Creates the EKS cluster (Terraform), then the GPU NodePool, then installs
# Anton with the vLLM backend. GPUs cost money from the moment a replica is
# scheduled: run down.sh when the work session ends.
#   deploy/eks/up.sh              cluster only (no GPU spend until replicas are scheduled)
#   INSTALL=1 deploy/eks/up.sh    ...and install Anton (images from GHCR or ECR)
set -euo pipefail
cd "$(dirname "$0")"
MAX_GPUS=${MAX_GPUS:-4}

aws sts get-caller-identity --query Account --output text
terraform init -upgrade >/dev/null
terraform apply -auto-approve -var "max_gpus=$MAX_GPUS"
$(terraform output -raw kubeconfig_command)

sed -e "s/__NAME__/$(terraform output -raw cluster_name)/" \
    -e "s/__NODE_ROLE__/$(terraform output -raw karpenter_node_role)/" \
    -e "s/__MAX_GPUS__/$MAX_GPUS/" -e "s/__GPU_TYPE__/${GPU_TYPE:-g6.xlarge}/" -e "s/__ZONE__/${WORKLOAD_ZONE:-us-east-2c}/" gpu-nodepool.yaml | kubectl apply -f -
kubectl get nodepools

if [ "${INSTALL:-0}" = "1" ]; then
  : "${IMAGE_REPO:?set IMAGE_REPO (e.g. ghcr.io/<owner>) and IMAGE_TAG}"
  helm upgrade --install anton ../helm/anton -n anton --create-namespace \
    --set image.repository="$IMAGE_REPO/anton" --set image.tag="$IMAGE_TAG" \
    --set sandboxImage.repository="$IMAGE_REPO/anton-sandbox" --set sandboxImage.tag="$IMAGE_TAG" \
    --set webImage.repository="$IMAGE_REPO/anton-web" --set webImage.tag="$IMAGE_TAG" \
    --set controlPlaneScheduling.nodeSelector."anton\.dev/role"=system --set controlPlaneScheduling.tolerations=null \
    --set inference.backend=vllm -f ../bench/profiles/qwen7b-l4/values.yaml ${HELM_ARGS:-} --wait --timeout 30m
fi
