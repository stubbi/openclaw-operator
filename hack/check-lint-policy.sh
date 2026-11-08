#!/usr/bin/env bash
# Exercise the full repository config so a disabled or excluded check fails CI.
# golangci-lint v1 enabled ST1003 and ST1016 even though standalone Staticcheck
# disabled them by default. Keep that policy when migrating linter versions.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
linter=$(command -v "${1:-golangci-lint}")
case "$linter" in
  /*) ;;
  *) linter="$PWD/$linter" ;;
esac

probe_dir=$(mktemp -d "${TMPDIR:-/tmp}/openclaw-lint-policy.XXXXXX")
trap 'rm -rf "$probe_dir"' EXIT

# Use the repository's minimum language version and the caller's Go toolchain.
go_version=$(awk '$1 == "go" { print $2; exit }' "$repo_root/go.mod")
printf 'module example.com/lintpolicy\n\ngo %s\n' "$go_version" > "$probe_dir/go.mod"
export GOWORK=off
cd "$probe_dir"

cat > probe.go <<'EOF'
package lintpolicy

var UserId = "example"

type Widget struct{ Value int }

func (w Widget) First() int { return w.Value }

func (other Widget) Second() int { return other.Value }
EOF

status=0
"$linter" run --config "$repo_root/.golangci.yaml" ./... > lint.log 2>&1 || status=$?
# Keep expected diagnostics captured: GitHub would annotate them as errors.
if [ "$status" -ne 1 ]; then
  cat lint.log
  echo "Expected lint violations (exit 1), got exit $status for invalid naming."
  exit 1
fi
for check in ST1003 ST1016; do
  if ! grep -q "$check:" lint.log; then
    cat lint.log
    echo "Missing expected $check diagnostic from the repository lint config."
    exit 1
  fi
done

cat > probe.go <<'EOF'
package lintpolicy

var UserID = "example"

type Widget struct{ Value int }

func (w Widget) First() int { return w.Value }

func (w Widget) Second() int { return w.Value }
EOF

"$linter" run --config "$repo_root/.golangci.yaml" ./...
echo "Lint policy verified: ST1003 and ST1016 reject violations; corrected code passes."
