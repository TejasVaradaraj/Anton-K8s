# Real-GPU Kubernetes on DigitalOcean (DOKS) for the end-to-end test:
# one CPU node for the control components and the cache tiers, and two
# RTX 4000 Ada GPU nodes for vLLM + LMCache replicas and agent sandboxes.
# Costs (2026-11): GPU nodes $0.76/h each; destroy right after the run.
# Token: export DIGITALOCEAN_TOKEN=$(cat <token file>)

terraform {
  required_version = ">= 1.9"
  required_providers {
    digitalocean = { source = "digitalocean/digitalocean", version = "~> 2.100" }
    random       = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

provider "digitalocean" {}

variable "region" {
  type    = string
  default = "tor1" # set to a region where your account can create RTX 4000 Ada
}

variable "gpu_nodes" {
  description = "RTX 4000 Ada workers. 0 until the CPU-side checks pass (no GPU spend)."
  type        = number
  default     = 0
}

variable "gpu_size" {
  type    = string
  default = "gpu-4000adax1-20gb"
}

data "digitalocean_kubernetes_versions" "v" {
  version_prefix = "1.37."
}

resource "digitalocean_kubernetes_cluster" "anton" {
  name    = "anton"
  region  = var.region
  version = data.digitalocean_kubernetes_versions.v.latest_version

  node_pool {
    name       = "system"
    size       = "s-8vcpu-16gb" # operator, router, API, Postgres, Anton tier, LMCache server
    node_count = 1
    labels     = { "anton.dev/role" = "system" }
  }

  # DOKS 1.37 no longer enables the NVIDIA device plugin by default.
  nvidia_gpu_device_plugin {
    enabled = true
  }
}

resource "digitalocean_kubernetes_node_pool" "gpu" {
  count      = var.gpu_nodes > 0 ? 1 : 0
  cluster_id = digitalocean_kubernetes_cluster.anton.id
  name       = "gpu"
  size       = var.gpu_size
  node_count = var.gpu_nodes
  labels     = { "anton.dev/gpu" = "true" }
}

resource "random_id" "registry" {
  byte_length = 3
}

# Images go here (nothing is on GitHub yet). Basic tier: room for ~5 GB.
resource "digitalocean_container_registry" "anton" {
  name                   = "anton-${random_id.registry.hex}"
  subscription_tier_slug = "basic"
  region                 = var.region
}

resource "digitalocean_container_registry_docker_credentials" "anton" {
  registry_name = digitalocean_container_registry.anton.name
}

output "cluster_id" { value = digitalocean_kubernetes_cluster.anton.id }
output "registry" { value = digitalocean_container_registry.anton.endpoint }
output "kubeconfig_command" { value = "doctl kubernetes cluster kubeconfig save ${digitalocean_kubernetes_cluster.anton.id}" }
