# Example silos

Complete silo folders for the [design](../design.md)'s first journeys,
and a grain-style headless run. Nothing here runs yet: they are written
against the schema in design §2.3, and stage 1's tests should parse
every one of them.

| Folder | Journey |
| --- | --- |
| [`create/`](create/) | 1: the built-in Silofile `silo create` runs (embedded in the binary). |
| [`google-project/`](google-project/) | 2: a local agent with `gcloud`, `gsutil` and `bq` on one project. |
| [`remote-scripts/`](remote-scripts/) | 3: an agent on a remote host over ssh, with two local scripts. |
| [`mcp-servers/`](mcp-servers/) | 4: existing MCP servers, local and remote. |
| [`grain-run/`](grain-run/) | A grain task run, headless, as grain would generate it. |

Sketches for journeys 5–10 are in design §14.
