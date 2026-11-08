#!/usr/bin/env bash
# Exercise the real Make test recipe without downloading tools or running Go tests.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat > "$tmp/bin/go" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  env) [ "$2" = GOPATH ] && printf '%s\n' "$TEST_TMP/gopath"; exit 0 ;;
  list) printf 'github.com/example/operator\n' ;;
  test) printf '%s\n' "${KUBEBUILDER_ASSETS-unset}" > "$TEST_TMP/tests-started" ;;
  *) echo "unexpected go invocation: $*" >&2; exit 1 ;;
esac
STUB
cat > "$tmp/bin/setup-envtest" <<'STUB'
#!/usr/bin/env bash
printf 'invoked\n' >> "$TEST_TMP/setup-calls"
case "$TEST_MODE" in
  failure) echo 'simulated download failure' >&2; exit 1 ;;
  empty) exit 0 ;;
  success) printf '%s\n' "$TEST_TMP/assets" ;;
esac
STUB
chmod +x "$tmp/bin/go" "$tmp/bin/setup-envtest"
export TEST_TMP="$tmp" PATH="$tmp/bin:$PATH"
run_make() {
  make -C "$repo" --no-print-directory -o manifests -o generate -o fmt -o vet \
    -o envtest -o test-plugin-installer -o test-envtest-recipe ENVTEST="$tmp/bin/setup-envtest" test
}
for TEST_MODE in failure empty; do
  export TEST_MODE
  rm -f "$tmp/tests-started" "$tmp/setup-calls"
  if run_make >"$tmp/output" 2>&1; then
    echo "FAIL: $TEST_MODE setup unexpectedly succeeded" >&2; cat "$tmp/output" >&2; exit 1
  fi
  if [ -e "$tmp/tests-started" ]; then
    echo "FAIL: Go tests started after $TEST_MODE setup" >&2; cat "$tmp/output" >&2; exit 1
  fi
  [ "$(wc -l < "$tmp/setup-calls" | tr -d ' ')" = 1 ] || { echo "FAIL: setup was not called once" >&2; exit 1; }
done
TEST_MODE=success; export TEST_MODE
rm -f "$tmp/tests-started" "$tmp/setup-calls"
run_make >"$tmp/output" 2>&1 || { cat "$tmp/output" >&2; exit 1; }
[ "$(cat "$tmp/tests-started")" = "$tmp/assets" ] || { echo 'FAIL: asset path was not forwarded' >&2; exit 1; }
[ "$(wc -l < "$tmp/setup-calls" | tr -d ' ')" = 1 ] || { echo 'FAIL: setup was not called once' >&2; exit 1; }
echo 'envtest Makefile regression passed'
