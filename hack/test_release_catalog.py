"""Execute both catalog preparation blocks without publishing anything."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
DIGEST = "sha256:" + "a" * 64


class CatalogPreparationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        shutil.copytree(ROOT / "bundle", self.root / "bundle")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.requests = self.root / "requests"
        self.env = {
            **os.environ,
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "REQUESTS": str(self.requests),
            "DOCKER_DIGEST": DIGEST,
            "DOCKER_STATUS": "0",
            "GITHUB_OUTPUT": str(self.root / "outputs"),
            "KUBECONFIG": "/dev/null",
        }
        self.command("docker", '''printf "%s\\n" "$*" >> "$REQUESTS"
if [ "${DOCKER_MANIFEST+x}" = x ]; then
  printf '%s\\n' "$DOCKER_MANIFEST"
else
  case "$*" in
    *'json .Manifest'*) python3 -c 'import json, os; print(json.dumps({"digest": os.environ["DOCKER_DIGEST"]}))' ;;
    *) printf 'Name: fixture\\nDigest: %s\\n' "$DOCKER_DIGEST" ;;
  esac
fi
exit "$DOCKER_STATUS"
''')

    def command(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/sh\nset -eu\n" + body)
        path.chmod(0o755)

    def prepare(self, job, tag):
        shutil.rmtree(self.root / "submission", ignore_errors=True)
        (self.root / "outputs").unlink(missing_ok=True)
        self.requests.unlink(missing_ok=True)
        workflow = yaml.safe_load((ROOT / ".github/workflows/operatorhub.yaml").read_text())
        step = next(s for s in workflow["jobs"][job]["steps"] if s.get("name") == "Prepare bundle")
        script = step["run"].replace("${{ steps.version.outputs.version }}", tag.removeprefix("v"))
        script = script.replace("${{ steps.version.outputs.tag }}", tag)
        return subprocess.run(["bash", "-e", "-c", script], cwd=self.root,
                              env=self.env, capture_output=True, text=True)

    def test_each_catalog_pins_all_csv_image_references(self):
        tag = "v0.40.0"
        repository = "ghcr.io/paperclipinc/openclaw-operator"
        for job in ["submit", "submit-redhat"]:
            with self.subTest(job=job):
                result = self.prepare(job, tag)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f"inspect {repository}:{tag} ", self.requests.read_text())
                csv = next((self.root / f"submission/operators/openclaw-operator/{tag[1:]}/manifests").glob("*.clusterserviceversion.yaml"))
                bundle = yaml.safe_load(csv.read_text())
                expected = f"{repository}@{DIGEST}"
                self.assertEqual(bundle["metadata"]["name"], "openclaw-operator." + tag)
                self.assertEqual(bundle["spec"]["version"], tag[1:])
                self.assertEqual(bundle["metadata"]["annotations"]["containerImage"], expected)
                self.assertEqual(bundle["spec"]["install"]["spec"]["deployments"][0]["spec"]["template"]["spec"]["containers"][0]["image"], expected)
                self.assertEqual(bundle["spec"]["relatedImages"][0]["image"], expected)
                self.assertTrue((csv.parent.parent / "metadata/annotations.yaml").is_file())
                self.assertGreater(len(list(csv.parent.glob("openclaw.rocks_*.yaml"))), 0)

    def test_each_catalog_rejects_missing_or_malformed_digests(self):
        for job in ["submit", "submit-redhat"]:
            for digest, status in [("", "1"), (DIGEST, "1"), ("", "0"), ("null", "0"), ("sha256:abc", "0"), ("<html>504</html>", "0"), (DIGEST + "\nextra=garbage", "0"), (DIGEST + "\n" + DIGEST, "0")]:
                with self.subTest(job=job, digest=digest, status=status):
                    self.env.update(DOCKER_DIGEST=digest, DOCKER_STATUS=status)
                    result = self.prepare(job, "v0.40.0")
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertFalse((self.root / "submission").exists())
                    self.assertFalse((self.root / "outputs").exists())

    def test_each_catalog_rejects_invalid_manifest_json(self):
        for job in ["submit", "submit-redhat"]:
            for payload in ["", "Name: fixture\nDigest: " + DIGEST, "{broken", "{}", '{"digest":null}']:
                with self.subTest(job=job, payload=payload):
                    self.env["DOCKER_MANIFEST"] = payload
                    result = self.prepare(job, "v0.40.0")
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertFalse((self.root / "submission").exists())
                    self.assertFalse((self.root / "outputs").exists())


if __name__ == "__main__":
    unittest.main()
