# CLAUDE.md — OpenClaw Kubernetes Operator

## Project Overview

Go-based Kubernetes operator for managing OpenClaw instances, built with controller-runtime (kubebuilder). CRD API group is `openclaw.rocks`, version `v1alpha1`.

- **Module:** `github.com/paperclipinc/openclaw-operator`
- **Go version:** 1.25 minimum; 1.27.3 for CI, container builds, and releases
- **GitHub:** `paperclipinc/openclaw-operator` (GHCR org: `paperclipinc`)

## Commands

```bash
make test          # Unit + integration tests (requires envtest binaries)
make lint          # golangci-lint
make build         # Build manager binary
make manifests     # Regenerate CRD YAML + RBAC after API type changes
make generate      # Regenerate deepcopy methods after API type changes
make install       # Install CRDs into current cluster
make run           # Run operator locally against current cluster
go test ./internal/resources/ -v   # Fast unit tests (no envtest needed)
go vet ./...       # Go vet check
```

## Architecture

```
api/v1alpha1/          → CRD types (OpenClawInstance)
internal/controller/   → Reconciliation logic (single controller)
internal/resources/    → Pure resource builder functions (Deployment, Service, etc.)
config/crd/bases/      → Generated CRD YAML (committed to git)
charts/                → Helm chart
bundle/                → OLM bundle for OperatorHub submissions
test/e2e/              → E2E tests (run against kind cluster)
```

**Separation of concerns:** Controller logic (`internal/controller/`) only orchestrates reconciliation. All resource construction happens in pure functions in `internal/resources/`. This makes builders easy to unit test without envtest.

## Reconciliation Rules

These rules are enforced by CI (Reconcile Guard check) and must be followed:

### Always use `controllerutil.CreateOrUpdate` for managed resources

Never call `r.Update()` or `r.Create()` directly on managed resources (Deployments, Services, ConfigMaps, etc.). Always use:

```go
obj := &appsv1.Deployment{
    ObjectMeta: metav1.ObjectMeta{
        Name:      resources.DeploymentName(instance),
        Namespace: instance.Namespace,
    },
}
_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
    desired := resources.BuildDeployment(instance)
    obj.Labels = desired.Labels
    obj.Spec = desired.Spec
    return controllerutil.SetControllerReference(instance, obj, r.Scheme)
})
```

**Why:** Direct `r.Update()` calls are unconditional — they update even when nothing changed, incrementing the resource generation, triggering a watch event, and causing an infinite reconciliation loop. `controllerutil.CreateOrUpdate` compares before/after and skips no-op updates.

**Exception:** `r.Update(ctx, instance)` on the CR itself is allowed for finalizer management. Add `// reconcile-guard:allow` for any other legitimate exceptions.

### Explicitly set all Kubernetes default values in builders

When building resources, always set fields that Kubernetes would default on the server side. If omitted, the desired spec differs from the stored spec on every reconcile.

**Deployment defaults to always set:**
- `RevisionHistoryLimit`, `ProgressDeadlineSeconds`
- `RestartPolicy`, `DNSPolicy`, `SchedulerName`, `TerminationGracePeriodSeconds`
- `TerminationMessagePath`, `TerminationMessagePolicy`, `ImagePullPolicy` on every container
- `SuccessThreshold` on every probe
- `DefaultMode` on ConfigMap volume sources

**Service defaults:** `SessionAffinity: None`

### Preserve server-assigned fields

When updating resources, preserve fields assigned by the API server:
- Service: `ClusterIP`, `ClusterIPs`
- PVC: immutable after creation — only create, never update

### Reconciliation return values

```go
return ctrl.Result{}, nil                    // Success, no requeue
return ctrl.Result{}, err                    // Requeue with exponential backoff
return ctrl.Result{Requeue: true}, nil       // Immediate requeue
return ctrl.Result{RequeueAfter: 5*time.Minute}, nil  // Requeue after delay
```

When returning `err != nil`, the `RequeueAfter` field is ignored (exponential backoff takes over).

### Status management

- Use `meta.SetStatusCondition` for conditions (follows k8s API conventions)
- Track `ObservedGeneration` so consumers know the controller processed the latest spec
- Status subresource updates (`r.Status().Update`) must be separate from spec/metadata updates

### Owner references

Set `controllerutil.SetControllerReference` on all managed resources. This enables:
- Automatic garbage collection when the parent CR is deleted
- Automatic watch triggers when owned resources change

## Coding Conventions

### Go style
- Use `0o644` (not `0644`) for octal literals - gocritic lint enforces this
- Wrap errors: `fmt.Errorf("context: %w", err)`
- Use the generic `Ptr[T]` helper from `internal/resources/common.go` for pointer values
- Never use em dashes or en dashes in code, comments, or strings - use regular hyphens/dashes (`-` or `--`) instead
- Run `make fmt` and `make lint` before committing

### Commit messages
Use conventional commits: `feat:`, `fix:`, `docs:`, `ci:`, `chore:`, `refactor:`, `test:`

The goreleaser changelog includes `feat:` and `fix:` only. Others are filtered out.

### Branch naming
Use prefixes: `feat/`, `fix/`, `chore/`, `docs/`, `ci/`, `refactor/`

Merged branches are auto-deleted. Always delete stale remote branches.

### Git worktrees
Always use `git worktree` when working on a separate branch to avoid switching branches and disrupting local state. Never use `git checkout` or `git switch` to change branches in the main working directory:

```bash
git worktree add ../openclaw-operator-<suffix> -b <branch> main
# work in the worktree directory, then clean up:
git worktree remove ../openclaw-operator-<suffix>
```

### CRD API changes
After modifying types in `api/v1alpha1/openclawinstance_types.go`:
1. Run `make generate` (regenerates `zz_generated.deepcopy.go`)
2. Run `make manifests` (regenerates CRD YAML in `config/crd/bases/`)
3. Commit the generated files

### Testing
- Resource builders: unit tests in `internal/resources/resources_test.go` (fast, no deps)
- Controller integration: envtest suite in `internal/controller/` (needs kubebuilder binaries)
- E2E: `test/e2e/` (needs kind cluster, runs in CI on PRs and main)
- **Always add e2e tests when feasible** -- any new feature or bug fix that changes the behavior of managed Kubernetes resources should include an e2e test verifying the resources are created correctly on a real cluster
- The `RawConfig` type embeds `runtime.RawExtension` -- in tests use:
  ```go
  instance.Spec.Config.Raw = &openclawv1alpha1.RawConfig{
      RawExtension: runtime.RawExtension{Raw: []byte(`{}`)},
  }
  ```

### Documentation

When adding or changing CRD fields, features, or behavior:

- **`README.md`** -- update the user-facing overview, examples, and the feature table.
- **`docs/api-reference.md`** is **auto-generated** from CRD types via `make api-docs`. Do NOT hand-edit it. After modifying types in `api/v1alpha1/`, run `make manifests api-docs` and commit the regenerated reference together with the type change. CI (`API Docs Sync` job) blocks any PR where running `make api-docs` would produce a diff.

The docs site (mkdocs-material) lives in `docs-site/`. See `docs-site/README.md` for local preview and contributor flow.

## CI Pipeline

All checks run on every push to main and every PR:

| Job | What it does |
|-----|-------------|
| **Lint** | golangci-lint v2.15.0 on Go 1.25 and 1.27.3 |
| **Test** | `make test` (unit + envtest integration) on Go 1.25 and 1.27.3 |
| **Security Scan** | gosec + Trivy (CRITICAL/HIGH) |
| **Reconcile Guard** | Grep check preventing bare `r.Update()` on managed resources |
| **Chart Image Repository** | Verifies the chart `image.repository` default points at the canonical `ghcr.io/paperclipinc` namespace (guards against the #536 stale-namespace regression) |
| **Helm RBAC Sync** | Verifies Helm chart ClusterRole contains all kubebuilder RBAC permissions |
| **Docs Build** | `mkdocs build --strict` against `docs-site/`; uploads preview artifact on PRs |
| **API Docs Sync** | Verifies `docs/api-reference.md` matches output of `make api-docs` |
| **Build** | Multi-arch Docker image (amd64 + arm64), pushes on main only |
| **E2E** | Kind cluster tests (PRs and main) |

## Security Practices

- **RBAC:** Use `+kubebuilder:rbac` markers with minimum required verbs. No wildcards.
- **Pod security:** Default to Restricted PSS — `runAsNonRoot`, drop `ALL` capabilities, seccomp RuntimeDefault
- **Secrets:** Operator has `get;list;watch;create;update;patch` on secrets — needed for auto-generating gateway token Secrets (owned by the CR, garbage-collected on deletion)
- **Images:** Signed with Cosign (keyless OIDC), SBOM attested
- **NetworkPolicy:** Enabled by default with deny-all baseline

## Helm Chart

- CRDs are Helm templates in `charts/openclaw-operator/templates/crds/` -- updated on every `helm upgrade`
- Run `make sync-chart-crds` after `make manifests` to sync CRDs into the Helm chart (CI enforces this)
- `appVersion` in `Chart.yaml` uses plain semver (no `v` prefix); the deployment template prepends `v`
- Chart version and appVersion are managed by release-please via `extra-files` in `release-please-config.json`

## Releasing

Automated via release-please + GoReleaser:

1. Conventional commits (`feat:`, `fix:`) on main trigger release-please to create/update a release PR
2. Merging the release PR bumps versions in `CHANGELOG.md`, `.release-please-manifest.json`, and `Chart.yaml`
3. A post-action step creates the `vX.Y.Z` tag (using PAT to trigger downstream workflows)
4. The tag triggers `release.yaml`: GoReleaser builds binaries + multi-arch Docker images (draft release)
5. Cosign signs images, SBOM is generated and attested
6. SBOM uploaded to draft release, then release is published (using PAT)
7. Published release triggers `operatorhub.yaml`: auto-submits bundle PR to `k8s-operatorhub/community-operators`
8. Helm chart is packaged and pushed to `oci://ghcr.io/paperclipinc/charts`

**Key config files:**
- `release-please-config.json` — `skip-github-release: true` (GoReleaser manages the release lifecycle)
- `.release-please-manifest.json` — tracks current version
- `.goreleaser.yaml` — `release.draft: true` (immutable releases require draft→publish flow)

**Secrets required:**
- `RELEASE_PLEASE_TOKEN` — classic PAT with `repo` scope; used for tag creation, release publishing, and OperatorHub cross-repo PRs

**Manual trigger:** `gh workflow run "OperatorHub Submission" -f tag=vX.Y.Z`

Image: `ghcr.io/paperclipinc/openclaw-operator`
