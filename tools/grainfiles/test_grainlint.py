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


if __name__ == "__main__":
    unittest.main()
