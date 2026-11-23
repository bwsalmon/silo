"""Archive/trust regressions; these never qualify a real worker."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
from bundle_common import sha
spec = importlib.util.spec_from_file_location("validate_bundle", SCRIPTS / "validate-bundle.py")
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class ExtractionTests(unittest.TestCase):
    def test_rejects_traversal_links_duplicates_and_privileged_modes(self):
        for kind in ["traversal", "link", "duplicate", "setuid"]:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                base = Path(directory)
                archive = base / "unsafe.tar.xz"
                with tarfile.open(archive, "w:xz") as tar:
                    member = tarfile.TarInfo("../escape" if kind == "traversal" else "bundle/file")
                    member.mode = 0o4755 if kind == "setuid" else 0o644
                    if kind == "link":
                        member.type = tarfile.SYMTYPE
                        member.linkname = "/etc/passwd"
                    tar.addfile(member)
                    if kind == "duplicate":
                        tar.addfile(member)
                with self.assertRaises(ValueError):
                    validator.extract(archive, base / "output", sha(archive))
                self.assertFalse((base / "output").exists())

    def test_checksum_is_required_before_extraction(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            path = base / "archive"
            path.write_bytes(b"untrusted input")
            with self.assertRaisesRegex(ValueError, "checksum"):
                validator.extract(path, base / "output", "0" * 64)
            self.assertFalse((base / "output").exists())


@unittest.skipUnless(os.environ.get("SILO_TEST_BUNDLE_ROOT"), "requires a built/extracted archive; not worker qualification")
class ActualBundleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="silo-bundle-test-")
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.root = self.base / "relocated with spaces"
        shutil.copytree(os.environ["SILO_TEST_BUNDLE_ROOT"], self.root)
        self.helper = self.root / "libexec/bwrap"

    def repin(self):
        path = self.root / "bundle.json"
        manifest = json.loads(path.read_text())
        manifest["files"]["libexec/bwrap"]["sha256"] = sha(self.helper)
        path.write_text(json.dumps(manifest))

    def test_relocation_and_malicious_path(self):
        attack = self.base / "attack"; attack.mkdir()
        marker = self.base / "executed"
        (attack / "bwrap").write_text("#!/bin/sh\necho bad > " + str(marker) + "\nexit 99\n")
        (attack / "bwrap").chmod(0o755)
        with patch.dict(os.environ, {"PATH":str(attack), "LD_LIBRARY_PATH":str(attack)}):
            result = validator.validate(self.root)
        self.assertEqual(result["bwrap"]["path"], str(self.helper))
        self.assertFalse(marker.exists())

    def test_missing_and_substituted_helper(self):
        self.helper.unlink()
        with self.assertRaisesRegex(ValueError, "missing"):
            validator.validate(self.root)
        self.helper.write_bytes(b"#!/bin/sh\nexit 0\n"); self.helper.chmod(0o755)
        with self.assertRaisesRegex(ValueError, "checksum"):
            validator.validate(self.root)

    def test_wrong_architecture_even_with_updated_pin(self):
        data = self.helper.read_bytes()
        self.helper.write_bytes(data[:18] + bytes([183, 0]) + data[20:])
        self.repin()
        with self.assertRaisesRegex(ValueError, "x86-64"):
            validator.validate(self.root)

    def test_bad_file_and_parent_modes(self):
        for mode in [0o644, 0o4755, 0o2755, 0o775, 0o777]:
            with self.subTest(mode=oct(mode)):
                self.helper.chmod(mode)
                with self.assertRaises(ValueError):
                    validator.validate(self.root)
        self.helper.chmod(0o755)
        self.root.chmod(0o777)
        with self.assertRaisesRegex(ValueError, "protected"):
            validator.validate(self.root)
        self.root.chmod(0o755)

    def test_symlink_helper_rejected(self):
        self.helper.unlink()
        self.helper.symlink_to(Path(os.environ["SILO_TEST_BUNDLE_ROOT"]) / "libexec/bwrap")
        with self.assertRaises(ValueError):
            validator.validate(self.root)

    def test_absent_library_fails_in_actual_loader(self):
        data = self.helper.read_bytes()
        self.assertIn(b"libcap.so.2", data)
        self.helper.write_bytes(data.replace(b"libcap.so.2", b"missing.so2"))
        self.repin()
        actual = subprocess.run([self.helper, "--version"], capture_output=True)
        self.assertNotEqual(actual.returncode, 0)
        self.assertIn(b"missing.so2", actual.stderr)
        with self.assertRaisesRegex(ValueError, "ELF"):
            validator.validate(self.root)


if __name__ == "__main__":
    unittest.main()
