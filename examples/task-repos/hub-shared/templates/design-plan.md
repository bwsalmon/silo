---
kind: template
schema: grain/template@v1
id: tmpl-4t2w0s3dn9f6
aka: ["repo:template:bwsalmon:grain:design-planner"]
name: Design-doc planner (abridged)
title: "Plan design doc changes: {{group}}"
inputs:
  - {name: group, type: string, required: true, label: The doc folder or file}
  - {name: paths, type: list, required: true}
  - {name: from_sha, type: string, required: true}
  - {name: to_sha, type: string, required: true}
  - {name: branch, type: string, default: main}
  - {name: mode, type: enum, options: [plan, verify], default: plan}
team: hub
---

A change to the design doc at `{{group}}` merged to `{{branch}}`.
{% if mode == "plan" -%}
Turn what is new or changed in it into implementation and test tasks.
{%- else -%}
Check the implementation against it, and file nothing.
{%- endif %}

- Range: `{{from_sha}}..{{to_sha}}` on `{{branch}}`.
- Files changed:
{%- for p in paths %}
  - `{{ p }}`
{%- endfor %}

{% with subject="what you planned", checks=["every task names its Design-Item", "no code changed"], strict=true -%}
{% include "partials/closing-report.md" %}
{%- endwith %}
