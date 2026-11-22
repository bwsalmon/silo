"""Deterministic reporting regressions, not worker qualification."""
import json
from pathlib import Path
import tempfile
import unittest

from qualification import finalization, verdict


class QualificationTests(unittest.TestCase):
    def test_partial_or_legacy_success_is_blocked(self):
        for suites in [[], ["runner"], ["runner", "network"]]:
            result = verdict({"result": "passed", "completed_suites": suites,
                              "cleanup": {"verified": True}})
            self.assertEqual(result["status"], "blocked")
            self.assertTrue(result["reasons"])

    def test_cleanup_failure_cannot_leave_passed_report(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.json"
            path.write_text('{"result":"passed"}')
            report = {"result": "checks_passed"}
            with self.assertRaisesRegex(RuntimeError, "unmount failed"):
                with finalization(report, path):
                    raise RuntimeError("unmount failed")
            written = json.loads(path.read_text())
            self.assertEqual(written["result"], "failed")
            self.assertFalse(written["cleanup"]["verified"])
            self.assertEqual(written["qualification"]["status"], "blocked")
            self.assertIn("unmount failed", written["cleanup"]["error"])

    def test_successful_cleanup_does_not_mask_failed_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.json"
            report = {"result": "failed", "error": "setup unavailable"}
            with finalization(report, path):
                pass
            written = json.loads(path.read_text())
            self.assertEqual(written["result"], "failed")
            self.assertTrue(written["cleanup"]["verified"])
            self.assertEqual(written["qualification"]["status"], "blocked")

    def test_absent_integration_evidence_blocks_qualification(self):
        report = {"result": "passed", "completed_suites": ["runner", "network", "volumes"],
                  "cleanup": {"verified": True}, "architecture": "x86_64", "kernel": "test-only"}
        self.assertEqual(verdict(report)["status"], "blocked")


if __name__ == "__main__":
    unittest.main()
