# Example silos

Complete silo folders for the [design](../design.md)'s first journeys,
and a grain-style headless run. Nothing here runs yet: they are written
against the schema in design §2.3, and `silofile`'s `TestExamples`
validates every one of them. Two are checked as what they are:
`create/` is a built-in silo's template (its `{{...}}` substituted, and
`builtin_tools` and the `folder` sandbox allowed), and `grain-run/` uses
values reserved for later stages, so `silo validate` refuses it with one
error per later value, each naming what adds it.

| Folder | Journey |
| --- | --- |
| [`create/`](create/) | 1: the built-in Silofile `silo create` runs (embedded in the binary). |
| [`google-project/`](google-project/) | 2: a local agent with `gcloud`, `gsutil` and `bq` on one project. |
| [`remote-scripts/`](remote-scripts/) | 3: an agent on a remote host over ssh, with two local scripts. |
| [`mcp-servers/`](mcp-servers/) | 4: existing MCP servers, local and remote. |
| [`grain-run/`](grain-run/) | A grain task run, headless, as grain would generate it. |

Sketches for journeys 5–10 are in design §14.
