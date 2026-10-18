# The Grainfile Format: a worked example

Two repositories that stand in for real ones (`stubs.yaml` maps each illustrative repository URL to its directory here). Validate them all with:

```sh
python3 tools/grainfiles/grainlint.py examples/grainfiles
```

See [`docs/grainfiles-spec.md`](../../docs/grainfiles-spec.md), Appendix A.

## `widgets/`: a code repository (`https://github.com/acme/widgets.git`)

- `docs/widget-schema.md` and `src/widget.go`: design documentation and Go source code referenced by tasks and processes.
- `.grain/tasks/2026-10-16-widget-schema-v2.task.md`: a discrete task requested by a user (`Bruce Salmon <bsalmon@google.com>`) and addressed to the `grain-eu` deployment (`git::https://github.com/acme/grain-eu.git`), referencing the design doc heading `/docs/widget-schema.md#schema-version-header` and `/src/widget.go`.
- `.grain/tasks/2026-10-16-widget-schema-v2-migration.task.md`: a follow-up task with `depends_on: ["./2026-10-16-widget-schema-v2.task.md"]`.
- `.grain/processes/schema-security-review.process.md`: a gating review process (`gate: true`) triggered on `pull_request` and `push` events touching `src/**`.
- `.grain/processes/release-cut-check.process.md`: a process triggered on a `scenario` condition and `manual: {}`.

## `grain-eu/`: a deployment repository (`https://github.com/acme/grain-eu.git`)

- `processes/qualification/smoke.process.md`: the lead step of a nightly release-qualification suite, with a deployment URL `requestor` and `recipient`, a cron `schedule` trigger in `Europe/Berlin`, a `manual` trigger, and a cross-repository `go-getter` reference to `widgets`.
- `processes/qualification/report.process.md`: the summary step of the qualification suite, grouped and ordered alongside `smoke.process.md` via `trigger_with: ["./smoke.process.md"]` and `depends_on: ["./smoke.process.md"]`.
