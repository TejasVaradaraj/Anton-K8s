#!/usr/bin/env bash
# Deletes everything up.sh created. GPU nodes go first (Karpenter would
# otherwise leave instances Terraform does not know about).
set -euo pipefail
cd "$(dirname "$0")"
aws sts get-caller-identity --query Account --output text
if kubectl get nodepool gpu >/dev/null 2>&1; then
  helm uninstall anton -n anton --wait || true
  kubectl delete nodepool gpu --wait=true || true
  kubectl delete ec2nodeclass gpu --wait=true || true
fi
terraform destroy -auto-approve
# Belt and braces: no Karpenter-launched instance may outlive the cluster.
LEFT=$(aws ec2 describe-instances --filters Name=tag:karpenter.sh/nodepool,Values=gpu Name=instance-state-name,Values=pending,running --query 'Reservations[].Instances[].InstanceId' --output text)
[ -z "$LEFT" ] && echo "no GPU instances remain" || { echo "terminating leftover GPU instances: $LEFT"; aws ec2 terminate-instances --instance-ids $LEFT >/dev/null; }
