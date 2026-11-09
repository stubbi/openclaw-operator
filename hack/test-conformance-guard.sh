#!/usr/bin/env bash
# Exercise the actual suite entry point without a cluster or user kubeconfig.
set -euo pipefail

cd "$(dirname "$0")/.."
go_cache=$(go env GOCACHE)
go_path=$(go env GOPATH)
go_mod_cache=$(go env GOMODCACHE)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/openclaw-conformance-guard.XXXXXX")
trap 'rm -rf "$fixture"' EXIT
mkdir "$fixture/home" "$fixture/bin" "$fixture/config-directory"
: > "$fixture/empty-config"
for command in kubectl helm kind; do
  cat > "$fixture/bin/$command" <<'SH'
#!/usr/bin/env sh
printf '%s\n' "$0 $*" >> "$CONFORMANCE_GUARD_CALLS"
exit 99
SH
  chmod +x "$fixture/bin/$command"
done
go test -c -o "$fixture/conformance.test" ./test/conformance

suite() {
  env HOME="$fixture/home" PATH="$fixture/bin:$PATH" \
    GOCACHE="$go_cache" GOPATH="$go_path" GOMODCACHE="$go_mod_cache" \
    CONFORMANCE_GUARD_CALLS="$fixture/calls" \
    KUBERNETES_SERVICE_HOST= KUBERNETES_SERVICE_PORT= KUBERNETES_MASTER= \
    CONFORMANCE_FULL="$1" KUBECONFIG="$2" \
    "$fixture/conformance.test" -test.run '^TestConformance$' -test.count=1 -test.v -test.timeout=2m \
      -ginkgo.v -ginkgo.no-color \
    > "$fixture/output" 2>&1
}

for config in '' "$fixture/missing-config" "$fixture/empty-config" "$fixture/config-directory"; do
  if suite 1 "$config"; then
    cat "$fixture/output" >&2
    echo "explicit full mode accepted missing or unusable kubeconfig: $config" >&2
    exit 1
  fi
  if ! grep -Fq 'CONFORMANCE_FULL=1 requires an explicit readable, non-empty KUBECONFIG file' "$fixture/output"; then
    cat "$fixture/output" >&2
    echo 'suite did not fail at the configuration guard' >&2
    exit 1
  fi
  test ! -e "$fixture/calls"
done
echo 'PASS: all four explicit full-mode failures stop at the guard'

if ! suite '' '' ||
  ! grep -Fq 'KUBECONFIG not set: conformance suite requires a live kind cluster' "$fixture/output" ||
  ! grep -Fq 'Suite skipped in BeforeSuite' "$fixture/output"; then
  cat "$fixture/output" >&2
  echo 'ordinary local mode did not preserve its safe skip' >&2
  exit 1
fi
test ! -e "$fixture/calls"
echo 'PASS: ordinary local mode still skips without kubeconfig'

env HOME="$fixture/home" PATH="$fixture/bin:$PATH" \
  GOCACHE="$go_cache" GOPATH="$go_path" GOMODCACHE="$go_mod_cache" \
  CONFORMANCE_GUARD_CALLS="$fixture/calls" KUBECONFIG= CONFORMANCE_FULL= \
  "$fixture/conformance.test" -test.run '^TestConformanceConfigurationGuard$' -test.count=1 -test.v \
  > "$fixture/positive-output" 2>&1
cat "$fixture/positive-output"
grep -Fq -- '--- PASS: TestConformanceConfigurationGuard' "$fixture/positive-output"
test ! -e "$fixture/calls"
echo 'PASS: owned non-empty configuration is accepted without creating a client'
