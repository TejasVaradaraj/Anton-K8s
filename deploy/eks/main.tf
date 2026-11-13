# EKS for Anton's real-GPU runs. Cost choices, in order of impact:
#   - no NAT gateway: nodes sit in public subnets (ingress still closed by
#     security groups), saving ~$33/month that would accrue even when idle;
#   - one small on-demand system node; GPUs only through Karpenter, spot only,
#     capped at var.max_gpus;
#   - tear it all down between work sessions (terraform destroy, ~10 min).

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  # Two zones for the EKS control plane (required); workloads use one of them.
  azs = distinct(concat([var.workload_zone], [for z in data.aws_availability_zones.available.names : z if z != var.workload_zone]))
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 6.7"

  name                    = var.name
  cidr                    = "10.42.0.0/16"
  azs                     = slice(local.azs, 0, 2)
  public_subnets          = ["10.42.0.0/20", "10.42.16.0/20"]
  map_public_ip_on_launch = true
  enable_nat_gateway      = false

  public_subnet_tags = {
    "kubernetes.io/role/elb"   = 1
    "karpenter.sh/discovery"   = var.name
  }
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 21.29"

  name                                     = var.name
  kubernetes_version                       = var.kubernetes_version
  endpoint_public_access                   = true
  enable_cluster_creator_admin_permissions = true

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.public_subnets

  # Cost: no KMS key or control-plane log group for a short-lived test cluster.
  create_kms_key              = false
  encryption_config           = null
  create_cloudwatch_log_group = false
  enabled_log_types           = []

  addons = {
    coredns                = {}
    kube-proxy             = {}
    vpc-cni                = { before_compute = true }
    eks-pod-identity-agent = { before_compute = true }
    # Real PVC workspaces and VolumeSnapshots (the kind setup can only fake
    # cross-node volumes).
    aws-ebs-csi-driver  = { pod_identity_association = [{ role_arn = module.ebs_csi_pod_identity.iam_role_arn, service_account = "ebs-csi-controller-sa" }] }
    snapshot-controller = {}
  }

  eks_managed_node_groups = {
    system = {
      instance_types = [var.system_instance_type]
      capacity_type  = "ON_DEMAND"
      min_size       = 1
      max_size       = 1
      desired_size   = 1
      labels         = { "anton.dev/role" = "system" }
      # The AMI's 20 GB root disk cannot hold the LMCache server image
      # (lmcache/vllm-openai, about 9 GB compressed).
      block_device_mappings = {
        xvda = {
          device_name = "/dev/xvda"
          ebs         = { volume_size = 80, volume_type = "gp3", delete_on_termination = true }
        }
      }
      # Same zone as the GPU nodes (see var.workload_zone).
      subnet_ids = [module.vpc.public_subnets[index(local.azs, var.workload_zone)]]
    }
  }

  node_security_group_tags = { "karpenter.sh/discovery" = var.name }
}

# IAM for the EBS CSI driver through EKS Pod Identity (no OIDC trust policy to manage).
module "ebs_csi_pod_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 2.10"

  name                      = "${var.name}-ebs-csi"
  attach_aws_ebs_csi_policy = true
}

# Images for the run (nothing is on GitHub yet). Deleted with the cluster.
resource "aws_ecr_repository" "anton" {
  for_each             = toset(["anton", "anton-sandbox", "anton-web"])
  name                 = each.key
  force_delete         = true
  image_tag_mutability = "MUTABLE"
}
