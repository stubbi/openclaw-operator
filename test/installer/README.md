# Verified plugin installer tests

`make test-plugin-installer` requires Node.js 22+ and runs the embedded installer against mocked registry responses and a disposable CLI executable. `make test` includes this suite. Tests cover byte and metadata integrity, origin/redirect rejection, deadlines and size bounds, malformed pins, explicit consent, duplicate installation directories, failed CLI execution, disabled lifecycle scripts, and scratch cleanup. They do not establish compatibility with the real OpenClaw CLI.

For real CLI compatibility, use the following opt-in smoke test. It needs Docker and network access to the image and public npm registries. It creates disposable in-memory home and scratch directories, passes no credentials, and removes its container on exit. Choose a reviewed plugin that requires capability consent. The test first reproduces missing consent, then verifies recovery and reinstallation with consent.

```sh
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
  --user 1000:1000 \
  --tmpfs /home/openclaw:uid=1000,gid=1000,mode=0700 \
  --tmpfs /tmp:uid=1000,gid=1000,mode=0700 \
  -e HOME=/home/openclaw \
  -e NPM_CONFIG_PREFIX=/home/openclaw/.local \
  -e NPM_CONFIG_CACHE=/home/openclaw/.cache/npm \
  -e NPM_CONFIG_IGNORE_SCRIPTS=true \
  -v "$PWD/internal/resources/scripts/install-verified-plugins.mjs:/installer.mjs:ro" \
  -v "$PWD/test/installer/runtime-smoke.mjs:/runtime-smoke.mjs:ro" \
  --entrypoint node \
  ghcr.io/openclaw/openclaw:2026.9.4@sha256:cc596b846506a5f4cfcee111394a2725f375f01cca2ebb492a161fd1b747f101 \
  /runtime-smoke.mjs /installer.mjs \
  '[{"package":"@openclaw/brave-plugin","version":"2026.9.1","integrity":"sha512-4+j+eQTToV3k7Cb25MUL6h2uL8cJYyuLytfpd/sJK/HjR43dgKBqKpBsb1+I3w1Jr6PLpnjSf6/I3//3K0cdnA=="}]'
```

Repeat against the intended runtime image when upgrading pins. These tests exercise install-time guarantees, not a dependency lock, runtime sandbox, or protection against later PVC mutation.

## Isolated Kubernetes test

The opt-in `Verified plugin installation` E2E spec exercises the modified operator, real init containers, PVC persistence, and a running gateway. It verifies that a wrong digest and missing consent prevent startup, then checks a valid installation through the gateway's `plugins.inspect` RPC before and after pod replacement. It supplies `gateway.bind: lan` explicitly for compatibility with the pinned runtime and uses no provider credentials.

The PVC state directory must be owned by the runtime UID (1000 in this fixture): OpenClaw 2026.9.4 tightens directory permissions during both verified and legacy plugin installs, which fails on a root-owned volume even when it is group-writable. The operator now prepares this ownership by default through `init-data-owner` (`spec.storage.fixOwnership`). If that feature is disabled, prepare ownership out of band before running this test.

Run it in a dedicated cluster with its own kubeconfig. For example, with Kind, Docker, Helm, kubectl, and Go installed:

```sh
verified_kubeconfig=$(mktemp)
kind create cluster --name verified-plugin-e2e --kubeconfig "$verified_kubeconfig"
docker build --build-arg TARGETARCH="$(go env GOARCH)" -t openclaw-operator:verified-test .
kind load docker-image openclaw-operator:verified-test --name verified-plugin-e2e
helm upgrade --install verified-operator charts/openclaw-operator \
  --kubeconfig "$verified_kubeconfig" --namespace verified-operator --create-namespace \
  --set image.repository=openclaw-operator --set image.tag=verified-test \
  --set image.pullPolicy=Never --wait
KUBECONFIG="$verified_kubeconfig" E2E_VERIFIED_PLUGINS=true \
  go test ./test/e2e -run TestE2E -v -ginkgo.focus='Verified plugin installation' -count=1 -timeout 20m
kind delete cluster --name verified-plugin-e2e --kubeconfig "$verified_kubeconfig"
rm "$verified_kubeconfig"
```

The spec creates and deletes its own namespace. Delete the dedicated cluster after testing, including after an unsuccessful run. This test does not make paid provider calls; the gateway RPC checks its effective plugin inventory and enablement, not a Brave search response.
