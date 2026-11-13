# Karpenter launches the GPU nodes, handles real spot interruption notices
# (through its SQS interruption queue) and consolidates underused nodes: the
# disruptions Anton exists to survive, now from the real cloud.

module "karpenter" {
  source  = "terraform-aws-modules/eks/aws//modules/karpenter"
  version = "~> 21.29"

  cluster_name                    = module.eks.cluster_name
  create_pod_identity_association = true
  node_iam_role_additional_policies = {
    ssm = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
  }
}

resource "helm_release" "karpenter" {
  name             = "karpenter"
  namespace        = "kube-system"
  repository       = "oci://public.ecr.aws/karpenter"
  chart            = "karpenter"
  version          = var.karpenter_version
  wait             = true

  values = [yamlencode({
    nodeSelector = { "anton.dev/role" = "system" }
    settings = {
      clusterName       = module.eks.cluster_name
      clusterEndpoint   = module.eks.cluster_endpoint
      interruptionQueue = module.karpenter.queue_name
    }
    controller = { resources = { requests = { cpu = "200m", memory = "512Mi" } } }
  })]
}

# NVIDIA device plugin: advertises nvidia.com/gpu on the GPU nodes.
resource "helm_release" "nvidia_device_plugin" {
  name       = "nvidia-device-plugin"
  namespace  = "kube-system"
  repository = "https://nvidia.github.io/k8s-device-plugin"
  chart      = "nvidia-device-plugin"
  version    = "0.17.1"
  values = [yamlencode({
    nodeSelector = { "anton.dev/gpu" = "true" }
    tolerations  = [{ key = "nvidia.com/gpu", operator = "Exists", effect = "NoSchedule" }]
  })]
}

# The GPU EC2NodeClass and NodePool are applied by up.sh with kubectl after this
# apply: kubernetes_manifest needs Karpenter's CRDs at plan time, which a fresh
# cluster does not have yet. See gpu-nodepool.yaml.
