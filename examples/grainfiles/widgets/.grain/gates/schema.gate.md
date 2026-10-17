---
title: Schema changes need a person
match:
  paths: ["src/widget.go", ".grain/designs/**"]
  branches: [main]
requires:
  human: {when: always}
  security_review: {template: "../templates/security-review.template.md"}
sensitive: true
---
A change to the widget schema always waits for a human review and a
security review before it merges.
