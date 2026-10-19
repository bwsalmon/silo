---
grainfile: v1
title: Provision a short-lived GCP service-account key in the sandbox
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
# GCP Service-Account Key Capability (`gcp-key`)

Grants a short-lived Google Cloud service-account credential to a task or process sandbox.

## Provisioning

Deployments that offer `gcp-key` mint a scoped token before launching the sandbox and mount it at `$GOOGLE_APPLICATION_CREDENTIALS`.

## Agent Usage

Authenticate with Application Default Credentials using the mounted key file at `$GOOGLE_APPLICATION_CREDENTIALS`.
