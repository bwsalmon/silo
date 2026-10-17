# The Grainfile Format: worked example

The worked example of [`docs/grainfiles-spec.md`](../../docs/grainfiles-spec.md)
Appendix A. Each top-level folder stands in for one repository on a forge;
[`stubs.yaml`](stubs.yaml) maps their illustrative URLs to these folders.

| Folder | Spec | What it shows |
| --- | --- | --- |
| [`hub-intake/`](hub-intake/) | A.1, A.5 | A team intake in a subfolder (`intake/`) of the `acme/hub` repo: a **custom lifecycle**, a **definition with its own thread** (`2026-10-16-approval-flow.task/`), the tombstone of a definition that moved to a code repo, and a **folder template** (grain's release-qualification suite, converted). Definitions hold no state. |
| [`widgets/`](widgets/) | A.2 | A **code repo** with a grain folder at `.grain/` (`role: library`): a gate, a single-task template and a definition homed there, its thread inside it. Its `src/` is outside every grain folder. |
| [`grain-eu/`](grain-eu/) | A.3, A.5, A.6 | Deployment `eu`'s root grain folder, with two lanes. `hub/2026-10-16-approval-flow.exec/` is an **execution** with its snapshot, its own thread and **two runs**; `hub/2026-10-16-widget-schema-v2.exec/` is **superseded** by a re-placement; `hub/release-qualification-2026-10-17/` is the folder template's **instance**, one execution per member definition; `backlog/` holds an execution of eu's own definition, with a run awaiting a reply. |
| [`grain-us/`](grain-us/) | A.6 | Deployment `us`, holding the **re-placed** execution (`previous:` names eu's). |
| [`registry/`](registry/) | A.4 | The hub's **registry**, itself a grain folder (`role: registry`). |

Check them all:

```sh
sudo apt-get install -y python3-yaml python3-jsonschema python3-jinja2
python3 tools/grainfiles/grainlint.py examples/grainfiles
python3 tools/grainfiles/grainlint.py --instantiate examples/grainfiles \
  examples/grainfiles/hub-intake/intake/templates/release-qualification/release-qualification.template.md candidate=v1.2
python3 tools/grainfiles/grainlint.py --self-test   # also every broken fixture
```
