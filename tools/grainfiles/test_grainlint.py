#!/usr/bin/env python3
"""Unit tests for grainlint.py (.task.grain.md, .process.grain.md, and .jinja2 templates)."""
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import grainlint


class TestGrainlint(unittest.TestCase):
    def test_self_test(self):
        self.assertEqual(grainlint.self_test(), 0)

    def test_plain_grain_md_is_literal(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = pathlib.Path(tmp) / "literal.task.grain.md"
            p.write_text(
                "---\n"
                "grainfile: v1\n"
                "title: Literal {{ not_a_var }}\n"
                "---\n"
                "Run `{{ literal_code }}` and {% if not_evaluated %}keep verbatim{% endif %}.\n",
                encoding="utf-8",
            )
            fm, body, tvars = grainlint.parse_grainfile(p, is_template=False)
            self.assertEqual(tvars, [])
            self.assertEqual(fm["title"], "Literal {{ not_a_var }}")
            self.assertIn("{{ literal_code }}", body)
            self.assertEqual(grainlint.lint(tmp), [])

    def test_jinja2_template_discovery_and_var_override(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "prep.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Prep task\n---\nPrepare.\n",
                encoding="utf-8",
            )
            (root / "deploy.task.grain.md.jinja2").write_text(
                "---\n"
                "grainfile: v1\n"
                "title: Deploy to {{ region | default('eu-west-1') | upper }}\n"
                "depends_on:\n"
                "  - {{ upstream | default('./prep.task.grain.md') }}\n"
                "---\n"
                "Deploy to {{ region | default('eu-west-1') }} in {{ env | default('staging') }}.\n",
                encoding="utf-8",
            )
            linter = grainlint.Linter(root)
            self.assertEqual(linter.run(), [])
            it = linter.items[(root, "deploy.task.grain.md.jinja2")]
            self.assertTrue(it.is_template)
            self.assertEqual(it.template_vars, ["env", "region", "upstream"])
            self.assertEqual(it.fm["title"], "Deploy to EU-WEST-1")

            linter_override = grainlint.Linter(root, cli_vars={"region": "us-west-2", "env": "prod"})
            self.assertEqual(linter_override.run(), [])
            it2 = linter_override.items[(root, "deploy.task.grain.md.jinja2")]
            self.assertEqual(it2.fm["title"], "Deploy to US-WEST-2")
            self.assertIn("prod", it2.body)

    def test_process_depends_on_rejected_with_gf020(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "a.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: A\n---\nRun A.\n",
                encoding="utf-8",
            )
            (root / "p.process.grain.md").write_text(
                "---\n"
                "grainfile: v1\n"
                "title: P\n"
                "depends_on:\n"
                "  - ./a.task.grain.md\n"
                "when:\n"
                "  manual: {}\n"
                "---\n"
                "Run P.\n",
                encoding="utf-8",
            )
            codes = [f.code for f in grainlint.lint(root)]
            self.assertEqual(codes, ["GF020"])

    def test_depends_on_unrendered_template_rejected_with_gf032(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "tmpl.task.grain.md.jinja2").write_text(
                "---\ngrainfile: v1\ntitle: T {{ x | default('1') }}\n---\nBody.\n",
                encoding="utf-8",
            )
            (root / "consumer.task.grain.md").write_text(
                "---\n"
                "grainfile: v1\n"
                "title: Consumer\n"
                "depends_on:\n"
                "  - ./tmpl.task.grain.md.jinja2\n"
                "---\n"
                "Run.\n",
                encoding="utf-8",
            )
            codes = [f.code for f in grainlint.lint(root)]
            self.assertEqual(codes, ["GF032"])

    def test_jinja2_forbidden_tags_and_syntax_errors(self):
        for tag in (
            '{% include "a.md" %}',
            '{% extends "base.md" %}',
            '{% import "m.md" as m %}',
            '{% from "m.md" import x %}',
        ):
            with self.assertRaises(grainlint.Finding) as ctx:
                grainlint.render_template("---\ntitle: T\n---\n" + tag)
            self.assertEqual(ctx.exception.code, "GF034")

        with self.assertRaises(grainlint.Finding) as ctx:
            grainlint.render_template("---\ntitle: {{ unclosed\n---\nBody.\n")
        self.assertEqual(ctx.exception.code, "GF033")

        for expr in (
            "{{ x | unknown_filter }}",
            "{{ 'a' + 'b' }}",
            "{{ 'a' ~ 'b' }}",
        ):
            with self.assertRaises(grainlint.Finding) as ctx:
                grainlint.render_template("---\ntitle: " + expr + "\n---\nBody.\n")
            self.assertEqual(ctx.exception.code, "GF033")

    def test_jinja2_expansion_cap_gf006(self):
        bomb = (
            "---\ntitle: Bomb\n---\n"
            "{% for a in ['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'] %}"
            "{% for b in ['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'] %}"
            + ("X" * 4200)
            + "{% endfor %}{% endfor %}\n"
        )
        with self.assertRaises(grainlint.Finding) as ctx:
            grainlint.render_template(bomb)
        self.assertEqual(ctx.exception.code, "GF006")

    def test_yaml12_quoted_specials_comments_and_null_keys(self):
        fm, body = grainlint.parse_text(
            "---\n"
            "# Comment with *alias and &anchor and !!str and <<:\n"
            'title: "Check *ptr and &ref and !!str and <<: in quotes"\n'
            "x_dots: '...'\n"
            "x_null_val: null\n"
            "---\n"
            "Body.\n",
            "t.task.grain.md",
        )
        self.assertIsNone(fm["x_null_val"])
        self.assertEqual(fm["x_dots"], "...")
        self.assertEqual(body, "Body.\n")

        for bad_key in ("null: 1", "Null: 1", "true: 1", "123: 1"):
            with self.assertRaises(grainlint.Finding) as ctx:
                grainlint.parse_text(
                    "---\ntitle: T\n" + bad_key + "\n---\nBody.\n",
                    "t.task.grain.md",
                )
            self.assertEqual(ctx.exception.code, "GF004")

    def test_parse_ref_encoded_traversal_and_remote_climbing(self):
        for bad_ref in (
            "./%2e%2e/secret.md",
            "/docs/%2E%2E/secret.md",
            "/docs/a%2fb.md",
            "https://github.com/acme/widgets/blob/main/docs%2fsecret.md",
            "https://github.com/acme/widgets/blob/main/docs%5csecret.md",
            "./a//b.md",
            "/docs//spec.md",
            "https://github.com/acme/widgets/blob/main/./spec.md",
        ):
            with self.assertRaises(grainlint.Finding) as ctx:
                grainlint.parse_ref(bad_ref)
            self.assertEqual(ctx.exception.code, "GF035", msg=bad_ref)

        with self.assertRaises(grainlint.Finding) as ctx:
            grainlint.parse_ref("https://github.com/acme/widgets/blob/main/docs/../../secret.md")
        self.assertEqual(ctx.exception.code, "GF036")

    def test_mixed_case_stems_and_gf005_only_on_grain_md(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            # Uppercase letters and ISO/compact UTC timestamps are valid stems in v1.
            (root / "2026-10-18T143000Z-fix-API.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Fix API\n---\nRun.\n",
                encoding="utf-8",
            )
            (root / "Big-Name.process.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Process\nwhen:\n  manual: {}\n---\nRun.\n",
                encoding="utf-8",
            )
            # Ordinary .task.md / .process.md files without .grain.md are regular Markdown assets (no GF005).
            (root / "notes.task.md").write_text("# Regular markdown note\n", encoding="utf-8")
            (root / "runbook.process.md").write_text("# Regular runbook\n", encoding="utf-8")
            self.assertEqual(grainlint.lint(root), [])

            # Unknown kind with .grain.md still warns with GF005.
            (root / "foo.unknown.grain.md").write_text("---\ntitle: X\n---\nBody.\n", encoding="utf-8")
            codes = [f.code for f in grainlint.lint(root)]
            self.assertEqual(codes, ["GF005"])

    def test_from_template_validation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "rollout.task.grain.md.jinja2").write_text(
                "---\ngrainfile: v1\ntitle: Rollout {{ region | default('eu') }}\n---\nDeploy.\n",
                encoding="utf-8",
            )
            (root / "audit.process.grain.md.jinja2").write_text(
                "---\ngrainfile: v1\ntitle: Audit\nwhen:\n  manual: {}\n---\nAudit.\n",
                encoding="utf-8",
            )
            # Valid same-repo and cross-repo from_template on task and process.
            (root / "eu-rollout.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: EU Rollout\nfrom_template: /rollout.task.grain.md.jinja2\n---\nDeploy EU.\n",
                encoding="utf-8",
            )
            (root / "us-rollout.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: US Rollout\nfrom_template: https://github.com/acme/templates/blob/main/rollout.task.grain.md.jinja2\n---\nDeploy US.\n",
                encoding="utf-8",
            )
            (root / "main-audit.process.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Main Audit\nfrom_template: ./audit.process.grain.md.jinja2\nwhen:\n  manual: {}\n---\nAudit main.\n",
                encoding="utf-8",
            )
            self.assertEqual(grainlint.lint(root), [])

        # Wrong suffix on task -> GF035
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "bad.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Bad\nfrom_template: /audit.process.grain.md.jinja2\n---\nBody.\n",
                encoding="utf-8",
            )
            self.assertEqual([f.code for f in grainlint.lint(root)], ["GF035"])

        # Missing local template -> GF030
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "missing.task.grain.md").write_text(
                "---\ngrainfile: v1\ntitle: Missing\nfrom_template: /nonexistent.task.grain.md.jinja2\n---\nBody.\n",
                encoding="utf-8",
            )
            self.assertEqual([f.code for f in grainlint.lint(root)], ["GF030"])

    def test_when_multi_repo_and_folder_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "gate.process.grain.md").write_text(
                "---\n"
                "grainfile: v1\n"
                "title: Multi-repo security gate\n"
                "gate: true\n"
                "when:\n"
                "  pull_request:\n"
                "    - repo: acme/payments\n"
                "      branches: [main, 'release/*']\n"
                "      paths:\n"
                "        - src/auth/\n"
                "        - /src/handler.go\n"
                "        - https://github.com/acme/payments/tree/main/src/billing\n"
                "      ignore:\n"
                "        - docs/\n"
                "    - repos:\n"
                "        - https://github.com/acme/widgets\n"
                "        - acme/checkout\n"
                "  push:\n"
                "    repo: https://github.com/acme/widgets\n"
                "---\n"
                "Review auth and billing edits.\n",
                encoding="utf-8",
            )
            self.assertEqual(grainlint.lint(root), [])

    def test_optional_empty_body_and_canonical_deployment_url(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            # Empty body after --- (with and without trailing newline) for .task.grain.md and .process.grain.md
            t1 = root / "empty-task.task.grain.md"
            t1.write_text(
                "---\ngrainfile: v1\ntitle: Empty body task\nrecipient: https://hub.example.com/deployments/payments-prod\n---\n",
                encoding="utf-8",
            )
            t2 = root / "empty-task-no-nl.task.grain.md"
            t2.write_text(
                "---\ngrainfile: v1\ntitle: Empty body task without trailing newline\nrecipient: /deployments/payments-prod\n---",
                encoding="utf-8",
            )
            p1 = root / "empty-process.process.grain.md"
            p1.write_text(
                "---\ngrainfile: v1\ntitle: Empty body process\nwhen:\n  manual: {}\n---\n",
                encoding="utf-8",
            )
            fm1, body1, _ = grainlint.parse_grainfile(t1)
            self.assertEqual(fm1["title"], "Empty body task")
            self.assertEqual(body1, "")
            fm2, body2, _ = grainlint.parse_grainfile(t2)
            self.assertEqual(fm2["recipient"], "/deployments/payments-prod")
            self.assertEqual(body2, "")
            fmp, bodyp, _ = grainlint.parse_grainfile(p1)
            self.assertEqual(fmp["title"], "Empty body process")
            self.assertEqual(bodyp, "")
            self.assertEqual(grainlint.lint(root), [])


if __name__ == "__main__":
    unittest.main()
