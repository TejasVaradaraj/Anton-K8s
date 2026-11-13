# Real spot interruptions on demand: an AWS FIS experiment that sends the
# 2-minute interruption notice to one running GPU spot node. Karpenter receives
# it through its queue, cordons and drains the node; Anton's guard rescues
# the replica's KV caches in that window. Run with:
#   aws fis start-experiment --experiment-template-id $(terraform output -raw fis_spot_template)

resource "aws_iam_role" "fis" {
  name = "${var.name}-fis"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "fis.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy" "fis" {
  role = aws_iam_role.fis.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = ["ec2:SendSpotInstanceInterruptions", "ec2:DescribeInstances"]
      Resource  = "*"
      Condition = { StringEquals = { "aws:ResourceTag/karpenter.sh/nodepool" = "gpu" } }
    }, {
      Effect   = "Allow"
      Action   = ["ec2:DescribeInstances"]
      Resource = "*"
    }]
  })
}

resource "aws_fis_experiment_template" "spot" {
  description = "Anton: spot interruption notice to one GPU node"
  role_arn    = aws_iam_role.fis.arn
  stop_condition { source = "none" }

  target {
    name           = "gpu-node"
    resource_type  = "aws:ec2:spot-instance"
    selection_mode = "COUNT(1)"
    resource_tag {
      key   = "karpenter.sh/nodepool"
      value = "gpu"
    }
    filter {
      path   = "State.Name"
      values = ["running"]
    }
  }

  action {
    name      = "interrupt"
    action_id = "aws:ec2:send-spot-instance-interruptions"
    parameter {
      key   = "durationBeforeInterruption"
      value = "PT2M"
    }
    target {
      key   = "SpotInstances"
      value = "gpu-node"
    }
  }
}
