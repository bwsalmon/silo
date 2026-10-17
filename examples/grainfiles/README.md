# The Grainfile Format: worked example

The worked example of [`docs/grainfiles-spec.md`](../../docs/grainfiles-spec.md)
Appendix A. Each top-level folder stands in for one repository on a forge;
[`stubs.yaml`](stubs.yaml) maps their illustrative URLs to these folders.

| Folder | Spec | What it shows |
| --- | --- | --- |
| [`grain-eu/`](grain-eu/) | A.1 | A **grain repo**: deployment `eu`'s root grain folder, `role: deployment`, with its own lane, a reference and its `.runs/` folder (a run and an agent comment), a trigger and memory. |
| [`widgets/`](widgets/) | A.2 | A **code repo** with a grain folder at `.grain/` (`role: library`): a gate, a single-task template and a task homed there. Its `src/` is outside every grain folder. |
| [`registry/`](registry/) | A.3 | The hub's **registry**, itself a grain folder (`role: registry`). |
| [`hub-intake/`](hub-intake/) | A.4, A.5 | A team intake in a subfolder (`intake/`) of the `acme/hub` repo, with a **custom lifecycle**, a task with its `.thread/` and `.placements/` folders, a **folder template** (grain's release-qualification suite, converted) and an **instance** of it. |

Check them all:

```sh
sudo apt-get install -y python3-yaml python3-jsonschema python3-jinja2
python3 tools/grainfiles/grainlint.py examples/grainfiles
python3 tools/grainfiles/grainlint.py --self-test   # also every broken fixture
```
