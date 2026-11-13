output "cluster_name" { value = module.eks.cluster_name }
output "kubeconfig_command" { value = "aws eks update-kubeconfig --name ${module.eks.cluster_name} --region ${var.region}" }
output "fis_spot_template" { value = aws_fis_experiment_template.spot.id }
output "karpenter_node_role" { value = module.karpenter.node_iam_role_name }
output "ecr_registry" { value = split("/", aws_ecr_repository.anton["anton"].repository_url)[0] }
output "workload_zone" { value = var.workload_zone }
