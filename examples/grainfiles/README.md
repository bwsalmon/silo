# Worked Example: `examples/grainfiles/`

Two repositories that stand in for real ones (`stubs.yaml` maps each illustrative repository URL to its directory here). Validate them all with:

```sh
python3 tools/grainfiles/grainlint.py examples/grainfiles
```

See [`docs/grainfile-v1.md`](../../docs/grainfile-v1.md) and [`docs/grainfiles-spec.md`](../../docs/grainfiles-spec.md), Appendix A.

## `widgets/`: a code repository (`https://github.com/acme/widgets`)

- `docs/widget-schema.md` and `src/widget.go`: design documentation and Go source code referenced by tasks and processes.
- `.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md`: a discrete `grainfile: v1` task with optional user attribution (`Bruce Salmon <bsalmon@google.com>`) addressed to the `grain-eu` deployment (`https://github.com/acme/grain-eu`), referencing the design doc heading `/docs/widget-schema.md#schema-version-header` and `/src/widget.go`.
- `.grain/tasks/2026-10-16-widget-schema-v2-migration.task.grain.md`: a follow-up task omitting optional `requestor`, with `depends_on: ["./2026-10-16-widget-schema-v2.task.grain.md"]` and `not_before: "2026-10-25T09:00:00Z"`.
- `.grain/tasks/2026-11-01-drop-widget-v1-support.task.grain.md`: a work-in-progress draft task (`draft: true`, `not_before: "2026-11-01"`) depending on the migration task; validated by `grainlint.py` while ignored by recipient deployments until `draft` is `false` or removed.
- `.grain/processes/schema-security-review.process.grain.md`: a reusable gating review process (`gate: true`) triggered via `when:` on `pull_request` and `push` events touching `src/**`.
- `.grain/processes/release-cut-check.process.grain.md`: a reusable process triggered via `when:` on a `scenario` condition and `manual: {}`.

## `grain-eu/`: a deployment repository (`https://github.com/acme/grain-eu`)

- `processes/qualification/smoke.process.grain.md`: the lead step of a nightly release-qualification suite, with a cron `when.schedule` trigger in `Europe/Berlin`, a `manual` trigger, and cross-repository `https://` `/tree/` and `/blob/` references to `widgets`.
- `processes/qualification/report.process.grain.md`: the summary step of the qualification suite, triggered on the same schedule (`when.schedule` and `when.manual`) and ordered after `smoke.process.grain.md` via `depends_on: ["./smoke.process.grain.md"]`.
