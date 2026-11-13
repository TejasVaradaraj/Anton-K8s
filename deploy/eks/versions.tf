terraform {
  required_version = ">= 1.9"
  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 6.0" }
    helm       = { source = "hashicorp/helm", version = "~> 3.0" }
  }
}

provider "aws" {
  region = var.region
  default_tags { tags = { Project = "anton", ManagedBy = "terraform" } }
}

# Helm and Kubernetes talk to the cluster created here, authenticating with
# the AWS CLI (no static credentials in state).
locals {
  exec_auth = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", var.region]
  }
}

provider "helm" {
  kubernetes = {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
    exec                   = local.exec_auth
  }
}
