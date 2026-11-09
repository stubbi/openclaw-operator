#!/usr/bin/env python3
"""Exercise the real Make target and workflow steps with isolated CLI fixtures."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
tool = Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["SIGNING_TEST_LOG"], "a") as out:
    out.write(json.dumps([tool, *args]) + "\\n")
if tool == "cosign":
    raise SystemExit(1 if os.environ.get("SIGNING_TEST_FAIL") == args[0] else 0)
if args[:2] == ["release", "view"]:
    if os.environ.get("SIGNING_TEST_FAIL") == "release":
        raise SystemExit(1)
    print(os.environ.get("SIGNING_TEST_TAG", "v0.40.0"))
elif args[:2] == ["issue", "list"]:
    if os.environ.get("SIGNING_TEST_FAIL") == "issue-list":
        raise SystemExit(1)
    print("42" if os.environ.get("SIGNING_TEST_EXISTING") == "1" else "")
elif args[:2] == ["issue", "create"]:
    if "--label" in args and args[args.index("--label") + 1] != "bug":
        raise SystemExit("unknown repository label")
    print("https://example.invalid/issues/42")
else:
    raise SystemExit("unexpected gh fixture call: " + repr(args))
'''


class VerifySigningTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name)
        self.log = self.path / "calls.jsonl"
        self.env = os.environ.copy()
        self.env.update(
            PATH=str(self.path) + os.pathsep + self.env["PATH"],
            SIGNING_TEST_LOG=str(self.log),
            GH_TOKEN="synthetic-test-token",
            GITHUB_OUTPUT=str(self.path / "output"),
            GITHUB_REPOSITORY="stubbi/openclaw-operator",
            GITHUB_RUN_ID="123",
            GITHUB_SERVER_URL="https://github.com",
        )
        self.env.pop("IMAGE_NAMESPACE", None)
        for tool in ("gh", "cosign"):
            file = self.path / tool
            file.write_text(FIXTURE)
            file.chmod(0o700)

    def calls(self, tool):
        return [x[1:] for line in self.log.read_text().splitlines()
                if (x := json.loads(line))[0] == tool] if self.log.exists() else []

    def target(self, tag="v0.40.0", namespace=None):
        self.env["SIGNING_TEST_TAG"] = tag
        command = ["make", "verify-signing"]
        if namespace is not None:
            command.append("IMAGE_NAMESPACE=" + namespace)
        return subprocess.run(
            command,
            cwd=ROOT, env=self.env, text=True, capture_output=True, timeout=30,
        )

    def workflow_step(self, name):
        workflow = yaml.safe_load((ROOT / ".github/workflows/verify-signing.yaml").read_text())
        step = next(x for x in workflow["jobs"]["verify"]["steps"] if x.get("name") == name)
        script = step["run"].replace("${{ github.repository }}", "stubbi/openclaw-operator")
        script = script.replace("${{ github.run_id }}", "123")
        return subprocess.run(
            ["bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script],
            cwd=ROOT, env=self.env, text=True, capture_output=True, timeout=30,
        )

    def assert_target(self, tag, namespace, expected):
        result = self.target(tag, namespace)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = self.calls("cosign")
        self.assertEqual([x[:2] for x in calls], [["verify", expected], ["verify-attestation", expected]])
        for call in calls:
            self.assertEqual(call[call.index("--certificate-oidc-issuer") + 1],
                             "https://token.actions.githubusercontent.com")
            policy = call[call.index("--certificate-identity-regexp") + 1]
            self.assertRegex("https://github.com/paperclipinc/openclaw-operator/.github/workflows/release.yaml@refs/tags/v0.40.0", policy)
            self.assertRegex("https://github.com/stubbi/openclaw-operator/.github/workflows/release.yaml@refs/tags/v0.40.0", policy)
            self.assertIsNone(re.search(policy, "https://github.com/attacker/openclaw-operator/.github/workflows/release.yaml@refs/tags/v0.40.0"))
            self.assertFalse(any(x.startswith("--insecure-ignore") for x in call))
        self.assertEqual(calls[1][calls[1].index("--type") + 1], "spdxjson")

    def test_default_namespace_matches_published_images(self):
        self.assert_target("v0.40.0", None, "ghcr.io/paperclipinc/openclaw-operator:v0.40.0")

    def test_explicit_custom_namespace_is_preserved(self):
        self.assert_target("v0.40.0", "custom-mirror", "ghcr.io/custom-mirror/openclaw-operator:v0.40.0")

    def test_release_lookup_failure_does_not_verify_an_empty_tag(self):
        self.env["SIGNING_TEST_FAIL"] = "release"
        self.assertNotEqual(self.target().returncode, 0)
        self.assertEqual(self.calls("cosign"), [])

    def test_empty_release_tag_is_rejected(self):
        self.assertNotEqual(self.target(tag="").returncode, 0)
        self.assertEqual(self.calls("cosign"), [])

    def test_bad_signature_fails_before_attestation(self):
        self.env["SIGNING_TEST_FAIL"] = "verify"
        self.assertNotEqual(self.target().returncode, 0)
        self.assertEqual([x[0] for x in self.calls("cosign")], ["verify"])

    def test_bad_attestation_fails(self):
        self.env["SIGNING_TEST_FAIL"] = "verify-attestation"
        self.assertNotEqual(self.target().returncode, 0)
        self.assertEqual([x[0] for x in self.calls("cosign")], ["verify", "verify-attestation"])

    def test_workflow_preserves_signature_failure(self):
        self.env["SIGNING_TEST_FAIL"] = "verify"
        self.assertNotEqual(self.workflow_step("Verify latest release").returncode, 0,
                            "signature failure became a successful workflow step")
        self.assertEqual([x[0] for x in self.calls("cosign")], ["verify"])

    def test_workflow_preserves_attestation_failure(self):
        self.env["SIGNING_TEST_FAIL"] = "verify-attestation"
        self.assertNotEqual(self.workflow_step("Verify latest release").returncode, 0,
                            "SBOM failure became a successful workflow step")

    def test_workflow_preserves_release_lookup_failure(self):
        self.env["SIGNING_TEST_FAIL"] = "release"
        self.assertNotEqual(self.workflow_step("Verify latest release").returncode, 0,
                            "release lookup failure became a successful workflow step")
        self.assertEqual(self.calls("cosign"), [])

    def test_workflow_preserves_empty_release_tag_failure(self):
        self.env["SIGNING_TEST_TAG"] = ""
        self.assertNotEqual(self.workflow_step("Verify latest release").returncode, 0,
                            "empty release tag became a successful workflow step")
        self.assertEqual(self.calls("cosign"), [])

    def test_successful_workflow_verifies_signature_and_sbom(self):
        result = self.workflow_step("Verify latest release")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([x[:2] for x in self.calls("cosign")], [
            ["verify", "ghcr.io/paperclipinc/openclaw-operator:v0.40.0"],
            ["verify-attestation", "ghcr.io/paperclipinc/openclaw-operator:v0.40.0"],
        ])

    def test_failure_reporting_uses_an_existing_label(self):
        result = self.workflow_step("Open issue on failure")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        creates = [x for x in self.calls("gh") if x[:2] == ["issue", "create"]]
        self.assertEqual(len(creates), 1)
        create = creates[0]
        self.assertEqual(create[create.index("--repo") + 1], "stubbi/openclaw-operator")
        self.assertEqual(create[create.index("--label") + 1], "bug")
        self.assertIn("https://github.com/stubbi/openclaw-operator/actions/runs/123",
                      create[create.index("--body") + 1])

    def test_failure_reporting_reuses_an_open_tracker(self):
        self.env["SIGNING_TEST_EXISTING"] = "1"
        result = self.workflow_step("Open issue on failure")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any(x[:2] == ["issue", "create"] for x in self.calls("gh")))

    def test_issue_lookup_failure_does_not_create_another_tracker(self):
        self.env["SIGNING_TEST_FAIL"] = "issue-list"
        self.assertNotEqual(self.workflow_step("Open issue on failure").returncode, 0)
        self.assertFalse(any(x[:2] == ["issue", "create"] for x in self.calls("gh")))


if __name__ == "__main__":
    unittest.main(verbosity=2)
