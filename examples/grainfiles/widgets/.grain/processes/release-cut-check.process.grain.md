---
grainfile: v1
title: Verify backwards compatibility when cutting a release branch
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/acme/grain-eu
when:
  scenario:
    description: "When a release candidate branch (release/*) is cut or cherry-picked"
  manual: {}
references:
  - /docs/widget-schema.md
  - /src/widget.go
---
Confirm that the release branch decodes both v1 and v2 widget fixtures
cleanly and that no breaking wire-format changes were introduced since
the previous tag.
