---
title: "Smoke-test {{ branch }} on {{ platforms | length }} platforms"
submitter: "@acme/release"
created_at: "2026-10-01T00:00:00Z"
context:
  - {ref: "git::https://github.com/acme/widgets.git?ref=candidate", mode: read}
---
Build `{{ branch }}` and run the smoke suite on:

{% for p in platforms %}
- `{{ p }}`{% if loop.last %} (last){% endif %}
{% endfor %}
{% include "partials/report-footer.md" %}
