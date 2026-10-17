# The Grainfile Format: a worked example

Three repositories that stand in for real ones (`stubs.yaml` maps each
illustrative URL to its folder here). Check them all with:

```
python3 tools/grainfiles/grainlint.py examples/grainfiles
```

See [`docs/grainfiles-spec.md`](../../docs/grainfiles-spec.md),
Appendix A.

## `commons/`: a shared library

- `grain.config.md`: `format: 1` and the folder's owner.
- `capabilities/gcp-key.capability/`: the `gcp-key` capability, an
  opaque name. `capability.md` has a title and a description;
  `assets/main.tf` is the Terraform that provisions the accounts behind
  it, which no grainfile reader interprets.
- `lifecycles/reviewed.lifecycle.md`: a state-machine definition that
  definitions may name.

Other folders pin commons through a versioned alias whose `sha256` is
the `h1:` hash of this tree (`grainlint.py --dirhash commons`).

## `widgets/`: a code repository with `.grain/`

- `.grain/grain.config.md`: pins commons as `commons:` at `v1.4.0`.
- `.grain/designs/widget-schema.design.md`: a design whose two
  requirements carry their hashes.
- `.grain/tasks/2026-10-16-widget-schema-v2.task/`: a definition that
  implements requirement WS-2 (its `implements` edge carries WS-2's
  hash) and requests `gcp-key`, with a `thread/` of two comments, one
  replying to the other.
- `.grain/gates/schema.gate.md`: a merge gate on the schema.
- `.grain/GRAINOWNERS`: forge identities; anyone may add a comment to a
  thread, and only `@acme/security` may change a definition's
  `capabilities`.

## `grain-eu/`: a deployment's folder

- `grain.config.md`: pins commons through the same versioned alias,
  makes pinning required, names the deployment's forge account
  `@acme-grain-eu`, offers `gcp-key`, and carries grain's own settings
  (opaque to the format).
- `templates/release-qualification/`: a folder template (grain's
  release-qualification suite): a smoke member that loops over a
  `platforms` list input and includes `partials/report-footer.md`, and
  a report member that depends on it. Render it with
  `grainlint.py --render examples/grainfiles grain-eu/templates/release-qualification/smoke.task.md platforms=…`.
- `templates/tidy.template.md`: a single-task template with an enum
  input.
- `triggers/nightly-qualification.trigger.md`: a cron trigger in a
  named time zone that fires the folder template.
- `records/`: two execution records written by `@acme-grain-eu`. One
  `succeeded`, with a pull-request link, ran the widgets definition at a
  pinned commit; one `failed`, ran the smoke member with its inputs and
  names the trigger. Each has its process log in `comments/`; the failed
  one also has a comment a person appended afterwards.

What eu's backlog holds, and the live state of anything it is running,
are in grain's own database: not in this format.
