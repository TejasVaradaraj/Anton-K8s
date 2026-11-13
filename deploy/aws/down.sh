#!/usr/bin/env bash
# Terminates the EC2 instance and removes the key pair and security group,
# so nothing is left running on the AWS bill.
set -euo pipefail
cd "$(dirname "$0")"
export AWS_DEFAULT_REGION="${AWS_REGION:-$(aws configure get region)}"

if [ -f .instance ]; then
  ID=$(cat .instance)
  aws ec2 terminate-instances --instance-ids "$ID" >/dev/null
  aws ec2 wait instance-terminated --instance-ids "$ID"
  rm -f .instance .ip
  echo "terminated $ID"
fi
aws ec2 delete-security-group --group-name anton-sg >/dev/null 2>&1 || true
aws ec2 delete-key-pair --key-name anton-key >/dev/null 2>&1 || true
rm -f anton-key.pem
LEFT=$(aws ec2 describe-instances --filters Name=tag:Name,Values=anton-bench Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text)
[ -z "$LEFT" ] && echo "no anton-bench instances remain" || echo "WARNING: still present: $LEFT"
