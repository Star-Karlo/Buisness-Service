# Cold storage, on a timer.
#
# Every night `server archive` runs as a one-off task from the service's own
# image and secrets — the same way migrations run on deploy — and moves
# orders, agreements and invoices that have been untouched for the retention
# window into Glacier Deep Archive. See internal/archive for what a run does
# and how something comes back.
#
# EventBridge Scheduler rather than a cron rule: it starts ECS tasks natively,
# with a network configuration and a command override, and retries a launch
# that ECS refused (capacity, throttling) without a Lambda in between.
#
# The task definition is named by FAMILY, not revision, so the schedule always
# runs whatever the last deploy registered. That is what keeps the archiver's
# code and the service's schema in step.

variable "archive_schedule" {
  description = "When the archiver runs. Cron in the schedule's own timezone; 02:00 Jakarta is after the day's last invoices and before the first dispatch."
  type        = string
  default     = "cron(0 2 * * ? *)"
}

variable "archive_enabled" {
  type    = bool
  default = true
}

data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
    # Only schedules in this account may assume it.
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_iam_role" "archive_scheduler" {
  name               = "${local.name}-archive-scheduler"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

data "aws_iam_policy_document" "archive_scheduler" {
  # Run this family and nothing else. The revision wildcard is what lets the
  # schedule follow deploys.
  statement {
    actions   = ["ecs:RunTask"]
    resources = ["${replace(aws_ecs_task_definition.main.arn, "/:\\d+$/", "")}:*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [local.platform.ecs_cluster_arn]
    }
  }
  # The task runs under the service's roles; the scheduler has to be allowed
  # to hand them over.
  statement {
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.task_execution.arn, aws_iam_role.task.arn]
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "archive_scheduler" {
  name   = "${local.name}-archive-scheduler"
  role   = aws_iam_role.archive_scheduler.id
  policy = data.aws_iam_policy_document.archive_scheduler.json
}

resource "aws_scheduler_schedule" "archive" {
  count = var.archive_enabled ? 1 : 0

  name        = "${local.name}-archive"
  description = "Nightly cold-storage run: server archive"

  schedule_expression          = var.archive_schedule
  schedule_expression_timezone = "Asia/Jakarta"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = local.platform.ecs_cluster_arn
    role_arn = aws_iam_role.archive_scheduler.arn

    ecs_parameters {
      # Family only: resolves to the latest ACTIVE revision at run time.
      task_definition_arn = replace(aws_ecs_task_definition.main.arn, "/:\\d+$/", "")
      launch_type         = "FARGATE"
      task_count          = 1

      network_configuration {
        subnets          = local.platform.tasks_in_public_subnets ? local.platform.public_subnet_ids : local.platform.private_subnet_ids
        security_groups  = [local.platform.tasks_security_group_id]
        assign_public_ip = local.platform.tasks_in_public_subnets
      }
    }

    # ECS appends the override to the image's ENTRYPOINT, which is the
    # binary, so this is ["archive"] and not ["server", "archive"].
    input = jsonencode({
      containerOverrides = [{
        name    = var.service_name
        command = ["archive"]
      }]
    })

    retry_policy {
      maximum_retry_attempts       = 2
      maximum_event_age_in_seconds = 3600
    }
  }
}

data "aws_caller_identity" "current" {}
