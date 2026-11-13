variable "region" {
  type    = string
  default = "us-east-2"
}

variable "name" {
  type    = string
  default = "anton"
}

# 1.37 is in standard support until 2027-11 (checked 2026-11-12). Versions in
# extended support cost 6x per hour; check with
#   aws eks describe-cluster-versions --query 'clusterVersions[].[clusterVersion,versionStatus]'
variable "kubernetes_version" {
  type    = string
  default = "1.37"
}

variable "system_instance_type" {
  description = "On-demand node for the control components, Postgres and the LMCache server."
  type        = string
  default     = "r7i.large" # 2 vCPU, 16 GiB
}

variable "gpu_instance_types" {
  description = "Spot GPU types Karpenter may launch for inference replicas."
  type        = list(string)
  default     = ["g6.xlarge"] # 1x L4 24 GB: the GPU the 7B calibration was measured on
}

variable "max_gpus" {
  description = "Hard cap on GPUs Karpenter will run at once (cost guard; the account's spot quota is 8 vCPUs = 2x g6.xlarge)."
  type        = number
  default     = 2
}

variable "workload_zone" {
  description = "All nodes run in this one zone, so EBS workspace volumes can follow a sandbox to another node."
  type        = string
  default     = "us-east-2c"
}

variable "karpenter_version" {
  type    = string
  default = "1.15.1" # released 2026-10-23
}
