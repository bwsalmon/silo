"""Deterministic reporting regressions, not worker qualification."""
from contextlib import ExitStack
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import real_worker

from qualification import finalization, verdict


class QualificationTests(unittest.TestCase):
    def complete_report(self):
        # Synthetic bookkeeping fixture; this never qualifies a real worker.
        return {"result": "passed", "completed_suites": ["runner", "network", "volumes"],
                "cleanup": {"verified": True}, "architecture": "x86_64", "kernel": "test-only",
                "agent_kernel_profiles": [{"test": True}], "resources": {"test": True},
                "postgres_platform_digest": "test-only", "network_direct_measurements": "test-only",
                "network_relay_measurements": "test-only", "pins": {"silo_sha256": "test-only"}}

    def test_partial_or_legacy_success_is_blocked(self):
        self.assertEqual(verdict(self.complete_report())["status"], "qualified")
        for suites in [[], ["runner"], ["runner", "network"], ["runner", "volumes"],
                       ["network", "volumes"]]:
            with self.subTest(suites=suites):
                report = self.complete_report()
                report["completed_suites"] = suites
                result = verdict(report)
                self.assertEqual(result["status"], "blocked")
                self.assertIn("runner, network and volumes suites are all required", result["reasons"])

    def test_early_setup_failure_invalidates_stale_success(self):
        for stage in ["namespace_policy", "workspace", "cgroup"]:
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "report.json"
                stale = self.complete_report()
                stale["qualification"] = verdict(stale)
                path.write_text(json.dumps(stale))
                make_workspace = tempfile.mkdtemp
                with ExitStack() as patches:
                    patches.enter_context(mock.patch.object(real_worker, "REPORT", {}))
                    patches.enter_context(mock.patch("sys.argv", ["real_worker.py", "--binary",
                                                                "/unused", "--report", str(path)]))
                    patches.enter_context(mock.patch.object(real_worker, "namespace_policy", return_value={}))
                    patches.enter_context(mock.patch.object(real_worker.os, "getuid", return_value=1000))
                    if stage == "namespace_policy":
                        patches.enter_context(mock.patch.object(real_worker, "namespace_policy",
                                                                side_effect=OSError("setup unavailable")))
                    if stage == "workspace":
                        patches.enter_context(mock.patch.object(real_worker.tempfile, "mkdtemp",
                                                                side_effect=OSError("setup unavailable")))
                    else:
                        patches.enter_context(mock.patch.object(real_worker.tempfile, "mkdtemp",
                            side_effect=lambda **kwargs: make_workspace(dir=directory, **kwargs)))
                    if stage == "cgroup":
                        patches.enter_context(mock.patch.object(Path, "read_text",
                                                                side_effect=OSError("setup unavailable")))
                    with self.assertRaisesRegex(OSError, "setup unavailable"):
                        real_worker.main()
                written = json.loads(path.read_text())
                self.assertEqual(written["result"], "failed")
                self.assertEqual(written["qualification"]["status"], "blocked")
                self.assertEqual(list(Path(directory).iterdir()), [path])

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
