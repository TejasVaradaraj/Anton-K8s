#!/usr/bin/env bash
# Creates ONE EC2 instance (m7i-flex.large, about $0.10/hour; the largest type the AWS free plan allows) that hosts
# the whole kind cluster. Run down.sh when finished so it stops costing money.
set -euo pipefail
cd "$(dirname "$0")"
REGION="${AWS_REGION:-$(aws configure get region)}"
TYPE="${INSTANCE_TYPE:-m7i-flex.large}"
export AWS_DEFAULT_REGION="$REGION"

if [ -f .instance ]; then echo "instance already recorded in deploy/aws/.instance; run down.sh first"; exit 1; fi

AMI=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
MYIP=$(curl -fsS https://checkip.amazonaws.com)

[ -f anton-key.pem ] || {
  aws ec2 delete-key-pair --key-name anton-key >/dev/null 2>&1 || true
  aws ec2 create-key-pair --key-name anton-key --query KeyMaterial --output text > anton-key.pem
  chmod 600 anton-key.pem
}
SG=$(aws ec2 describe-security-groups --group-names anton-sg --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)
if [ -z "$SG" ] || [ "$SG" = "None" ]; then
  SG=$(aws ec2 create-security-group --group-name anton-sg --description "Anton bench: SSH from my IP only" --query GroupId --output text)
fi
# SSH is open to this machine's current IP only.
# SSH is open to any address on purpose (owner's choice, 2026-11-13): the Mac
# changes networks, and an IP-restricted rule locked the session out mid-run.
# sshd on the box accepts keys only. Set SSH_CIDR to restrict it again.
aws ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr "${SSH_CIDR:-0.0.0.0/0}" >/dev/null 2>&1 || true

ID=$(aws ec2 run-instances --image-id "$AMI" --instance-type "$TYPE" --key-name anton-key \
  --security-group-ids "$SG" \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=60,VolumeType=gp3,DeleteOnTermination=true}' \
  --instance-initiated-shutdown-behavior terminate \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=anton-bench}]' \
  --query 'Instances[0].InstanceId' --output text)
echo "$ID" > .instance
aws ec2 wait instance-running --instance-ids "$ID"
IP=$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "$IP" > .ip
echo "instance $ID is running at $IP ($TYPE in $REGION)"
echo "next: deploy/aws/run.sh bench"
