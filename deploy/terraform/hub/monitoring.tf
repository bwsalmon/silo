# The hub's alert policies and dashboard, in Cloud Monitoring, over the
# metrics Managed Service for Prometheus scrapes from the hub's
# metrics port (the PodMonitoring in deploy/kubernetes/hub; the metrics
# are listed in docs/hub-operations.md). Each condition is PromQL, which
# Cloud Monitoring evaluates against the same data the dashboard charts.
#
# Cloud Monitoring rather than Grafana: the metrics already land in
# Cloud Monitoring, which alerts and charts them with nothing more to
# run, secure or upgrade, where Grafana would need its own deployment,
# sign-in, data-source credentials and an Alertmanager for its
# notifications. Policies, notification channels and the dashboard are
# then all terraform, in the project, beside the rest of the hub.
#
# hub/server's TestMonitoringNamesMetrics checks that every silo_hub_
# metric named here and in dashboards/ is one the hub exports.

locals {
  # The hub's scrape target: the PodMonitoring's name is the job.
  hub_selector = "namespace=\"${var.namespace}\",job=\"silo-hub\""

  alerts = {
    hub_down = {
      name     = "Silo hub down or not ready"
      severity = "CRITICAL"
      # No scrape at all (the pod is gone, or cannot start), a scrape
      # that fails, or a hub that answers but says it is not ready.
      query    = "absent_over_time(up{${local.hub_selector}}[5m]) or max(up{${local.hub_selector}}) == 0 or max(silo_hub_ready{${local.hub_selector}}) == 0"
      duration = "300s"
      doc      = "The hub has been down or unready for 5 minutes: nodes cannot connect, and no task is placed. See \"Hub down or not ready\" in docs/hub-operations.md."
    }
    no_online_nodes = {
      name     = "Silo team has queued work and no online node"
      severity = "WARNING"
      query    = "sum by (team) (silo_hub_backlog_tasks{${local.hub_selector},state=\"queued\"}) > 0 and on (team) sum by (team) (silo_hub_team_nodes{${local.hub_selector},status=\"online\"}) == 0"
      duration = "900s"
      doc      = "A team has had queued tasks for 15 minutes and no online node serves any of its repositories. See \"No online nodes\" in docs/hub-operations.md."
    }
    dispatch_stuck = {
      name     = "Silo dispatch stuck"
      severity = "WARNING"
      # Free slots to place on, and a task that has waited half an hour
      # anyway: the dispatcher is wedged, or every node with room
      # refuses the task (or fails its constraints).
      query    = "max by (team) (silo_hub_backlog_oldest_queued_seconds{${local.hub_selector}}) > 1800 and on (team) max by (team) (silo_hub_team_free_slots{${local.hub_selector}}) > 0"
      duration = "600s"
      doc      = "A team's oldest queued task has waited over 30 minutes while its nodes have free slots. See \"Dispatch stuck\" in docs/hub-operations.md."
    }
    error_rate = {
      name     = "Silo hub error rate"
      severity = "WARNING"
      # 5xx answers, but not the readiness check's while the hub drains,
      # the view-through's 503 for an offline node, which are by
      # design, or a node's own UI's (hub.md §11), which are the node's. At least one a minute, so a quiet hub does not page on
      # one error.
      query    = "(sum(rate(silo_hub_http_requests_total{${local.hub_selector},code=~\"5..\",route!~\".*(readyz|artifacts)|node-ui\"}[5m])) / sum(rate(silo_hub_http_requests_total{${local.hub_selector}}[5m]))) > 0.05 and sum(rate(silo_hub_http_requests_total{${local.hub_selector},code=~\"5..\",route!~\".*(readyz|artifacts)|node-ui\"}[5m])) > 1/60"
      duration = "300s"
      doc      = "Over 5% of the hub's requests have been answered 5xx for 5 minutes. See \"Error rate\" in docs/hub-operations.md."
    }
    no_dispatcher = {
      name     = "Silo hub not dispatching"
      severity = "CRITICAL"
      # A hub that is up while no pod holds the dispatcher's lease
      # (hub.md §2.2.1): its Recover keeps failing, or the lease is held
      # by a session that is not dispatching.
      query    = "max(up{${local.hub_selector}}) == 1 and sum(silo_hub_dispatcher_leader{${local.hub_selector}}) == 0"
      duration = "300s"
      doc      = "The hub has been up for 5 minutes with no pod running the dispatcher: no task is placed, resumed or cancelled. See \"Not dispatching\" in docs/hub-operations.md."
    }
    database_errors = {
      name     = "Silo hub database errors"
      severity = "CRITICAL"
      query    = "max(silo_hub_database_up{${local.hub_selector}}) == 0 or sum(rate(silo_hub_store_errors_total{${local.hub_selector}}[5m])) > 0"
      duration = "300s"
      doc      = "The hub's database has not answered, or its stores have failed, for 5 minutes. See \"Database errors\" in docs/hub-operations.md."
    }
  }
}

resource "google_monitoring_alert_policy" "hub" {
  for_each = var.monitoring ? local.alerts : {}

  project               = var.project
  display_name          = each.value.name
  combiner              = "OR"
  severity              = each.value.severity
  notification_channels = var.alert_notification_channels

  conditions {
    display_name = each.value.name
    condition_prometheus_query_language {
      query               = each.value.query
      duration            = each.value.duration
      evaluation_interval = "60s"
      labels = {
        alert = each.key
      }
    }
  }

  documentation {
    content   = each.value.doc
    mime_type = "text/markdown"
  }

  alert_strategy {
    auto_close = "1800s"
  }

  depends_on = [google_project_service.apis]
}

resource "google_monitoring_dashboard" "hub" {
  count   = var.monitoring ? 1 : 0
  project = var.project
  # The selector goes inside JSON strings, so its quotes are escaped.
  dashboard_json = templatefile("${path.module}/dashboards/hub.json", { selector = replace(local.hub_selector, "\"", "\\\"") })

  depends_on = [google_project_service.apis]
}
