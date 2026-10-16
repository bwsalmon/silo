---
kind: template
schema: grain/template@v1
id: tmpl-p6q9fceyh42c
name: Closing report
partial: true
inputs:
  - {name: subject, type: string, required: true, label: What the report is about}
  - {name: checks, type: list, default: []}
  - {name: strict, type: bool, default: false}
---

## Report

Finish with a summary of {{ subject | trim }}. It becomes this task's
`closed.summary`.
{% if checks %}
Before you report, confirm each of these:
{% for c in checks %}
{{ loop.index }}. {{ c }}
{%- endfor %}
{% endif %}
{%- if strict %}
If any check fails, end the task as failed rather than done.
{% endif -%}
