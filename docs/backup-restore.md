---
title: Backup and Restore
description: S3-backed PVC backup, scheduled backups, restore, clone/migrate workflows, and cloud-native Workload Identity authentication.
---

# Backup and Restore

The operator uses [rclone](https://rclone.org/) to sync PVC data to and from an S3-compatible backend. All backup operations are driven by a single Secret named `s3-backup-credentials` in the **operator namespace** (the namespace where the operator pod runs, typically `openclaw-operator-system`).

## S3 Credentials Secret

Create the Secret before enabling any backup or restore feature:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3-backup-credentials
  namespace: openclaw-operator-system  # must match the operator namespace
stringData:
  S3_ENDPOINT: "https://s3.us-east-1.amazonaws.com"   # or any S3-compatible URL
  S3_BUCKET: "my-openclaw-backups"
  S3_ACCESS_KEY_ID: "<key-id>"
  S3_SECRET_ACCESS_KEY: "<secret-key>"
  # S3_REGION: "us-east-1"  # optional - see below
```

`S3_ENDPOINT` and `S3_BUCKET` are required. The operator uses rclone's S3 backend, which is compatible with AWS S3, Backblaze B2, MinIO, Cloudflare R2, Wasabi, Google Cloud Storage (S3-compatible), and any other S3-compatible service.

| Key | Required | Description |
|-----|----------|-------------|
| `S3_ENDPOINT` | Yes | S3-compatible endpoint URL (e.g., `https://s3.us-east-1.amazonaws.com`) |
| `S3_BUCKET` | Yes | Bucket name for backups |
| `S3_ACCESS_KEY_ID` | No | Access key ID. When omitted (together with `S3_SECRET_ACCESS_KEY`), rclone uses `--s3-env-auth=true` to authenticate via the provider's native credential chain. |
| `S3_SECRET_ACCESS_KEY` | No | Secret access key. When omitted (together with `S3_ACCESS_KEY_ID`), rclone uses `--s3-env-auth=true`. |
| `S3_REGION` | No | S3 region (e.g., `us-east-1`). Required for MinIO instances configured with a custom region. Without this, rclone defaults to `us-east-1`, which causes authentication failures on providers using a different region. |
| `S3_PROVIDER` | No | rclone S3 provider (default: `Other`). Set to `AWS` for native AWS credential chain, `GCS` for Google Cloud Storage, `Ceph` for Ceph/RadosGW, etc. Setting the correct provider enables provider-specific auth flows and optimizations. See [rclone S3 providers](https://rclone.org/s3/#s3-provider). |

## When backups run automatically

| Trigger | Condition |
|---------|-----------|
| **Pre-delete backup** | Always runs when a CR is deleted, unless `openclaw.rocks/skip-backup: "true"` annotation is set or persistence is disabled. Subject to `spec.backup.timeout` (default: 30m) - if the backup does not complete within the timeout, it is skipped and deletion proceeds. |
| **Pre-update backup** | Runs before each auto-update when `spec.autoUpdate.backupBeforeUpdate: true` (the default). |
| **Periodic (scheduled)** | Runs on a cron schedule when `spec.backup.schedule` is set. See [Periodic / scheduled backups](#periodic-scheduled-backups) below. |

If the `s3-backup-credentials` Secret does not exist, pre-delete backups are skipped and deletion proceeds. Auto-updates requiring a backup are aborted and the old version is restarted. The periodic backup CronJob is not created (a `ScheduledBackupReady=False` condition is set with reason `S3CredentialsMissing`).

## Backup path format

Backups are stored at:

```
s3://<bucket>/backups/<tenantId>/<instanceName>/<timestamp>
```

Where:
- `<tenantId>` is the value of the `openclaw.rocks/tenant` label on the instance, or derived from the namespace (e.g., namespace `oc-tenant-abc` yields `abc`), or the namespace name itself.
- `<instanceName>` is `metadata.name` of the `OpenClawInstance`.
- `<timestamp>` is an RFC3339 timestamp at the time the backup job runs.

The path of the last successful backup is recorded in `status.lastBackupPath`.

## Backup timeout

Pre-delete backups are subject to a configurable timeout (default: 30 minutes). If the backup does not complete within the timeout -- whether due to a stuck Job, pod termination issues, or S3 credential errors -- the operator logs a warning, emits a `BackupTimedOut` event, sets the `BackupComplete=False` condition with reason `BackupTimedOut`, and proceeds with deletion.

Configure the timeout via `spec.backup.timeout`:

```yaml
spec:
  backup:
    timeout: "1h"  # Allow up to 1 hour for pre-delete backups (default: 30m, min: 5m, max: 24h)
```

## Skip backup on delete

To delete an instance immediately without waiting for a backup (e.g., the S3 backend is unavailable):

```bash
kubectl annotate openclawinstance my-agent openclaw.rocks/skip-backup=true
kubectl delete openclawinstance my-agent
```

## Restoring an instance

Set `spec.restoreFrom` to an existing backup path. The operator runs an rclone restore job to populate the PVC before starting the StatefulSet, then clears the field automatically:

```yaml
spec:
  restoreFrom: "backups/my-tenant/my-agent/2026-01-15T10:30:00Z"
```

To find available backups, list the S3 bucket directly (e.g., `aws s3 ls s3://my-openclaw-backups/backups/`). The `status.lastBackupPath` field on any existing instance shows its last backup path.

**Restore behavior:**

- The restore Job runs **before** the StatefulSet is created (reconcile order: PVC -> restore Job -> StatefulSet)
- `spec.restoreFrom` is cleared automatically after a successful restore and the original path is recorded in `status.restoredFrom`
- The restore Job uses `spec.backup.serviceAccountName` when set, so workload identity (IRSA/Pod Identity) works for restores
- If the restore Job fails, the operator sets `RestoreComplete=False` and retries. Delete the failed Job to trigger a fresh attempt

## Clone / migrate an instance

`spec.restoreFrom` works on **new instances** (with empty PVCs), not just existing ones. This enables cloning and cross-namespace migration workflows.

**Example - clone instance A from namespace X to namespace Y:**

```yaml
# 1. Check the source instance's last backup path:
#    kubectl get openclawinstance my-agent -n ns-x -o jsonpath='{.status.lastBackupPath}'
#    -> backups/tenant-x/my-agent/2026-03-15T02:00:00Z

# 2. Create a new instance in the target namespace with restoreFrom:
apiVersion: openclaw.rocks/v1alpha1
kind: OpenClawInstance
metadata:
  name: my-agent-clone
  namespace: ns-y
spec:
  image:
    repository: ghcr.io/openclaw/openclaw
    tag: latest
  restoreFrom: "backups/tenant-x/my-agent/2026-03-15T02:00:00Z"
  backup:
    serviceAccountName: "openclaw-backup"  # if using workload identity
```

The operator creates the PVC, runs the restore Job (rclone sync from S3 to the new PVC), then starts the StatefulSet with the restored data. The new instance gets a fresh gateway token - the source instance is unaffected.

**ArgoCD integration:** The operator auto-clears `spec.restoreFrom` after a successful restore. To prevent ArgoCD from detecting this as drift, add it to `ignoreDifferences`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  ignoreDifferences:
    - group: openclaw.rocks
      kind: OpenClawInstance
      jsonPointers:
        - /spec/restoreFrom
```

## Periodic / scheduled backups

Set `spec.backup.schedule` to a cron expression to enable periodic backups:

```yaml
spec:
  backup:
    schedule: "0 2 * * *"     # Daily at 2 AM UTC
    historyLimit: 3            # Successful job runs to retain (default: 3)
    failedHistoryLimit: 1      # Failed job runs to retain (default: 1)
```

The operator creates a Kubernetes CronJob (`<instance>-backup-periodic`) that:

- Mounts the PVC with **fsGroup** matching the StatefulSet (hot backup - no downtime or StatefulSet scale-down)
- Uses **pod affinity** to co-locate on the same node as the StatefulSet pod (required for RWO PVCs)
- Stores each run under a unique timestamped path: `backups/<tenantId>/<instanceName>/periodic/<YYYYMMDDTHHMMSSz>`
- Uses `ConcurrencyPolicy: Forbid` to prevent overlapping backup runs
- Runs with the same rclone image and security context (UID/GID 1000) as on-delete backups

**Requirements:** persistence must be enabled and the `s3-backup-credentials` Secret must exist. If either is missing, the CronJob is not created and a `ScheduledBackupReady=False` condition is set.

**Removing the schedule:** set `spec.backup.schedule` to an empty string (or remove the `backup` section entirely) and the CronJob is automatically deleted.

## Workload Identity (cloud-native auth)

Instead of static credentials, you can use your cloud provider's workload identity to authenticate backup Jobs:

- **AWS EKS**: [IRSA](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html) or [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html) with `S3_PROVIDER=AWS`
- **GKE**: [Workload Identity Federation](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) with `S3_PROVIDER=GCS` (using GCS S3-compatible endpoint)
- **AKS**: [Workload Identity](https://learn.microsoft.com/en-us/azure/aks/workload-identity-overview) with static HMAC keys or a compatible S3 provider

The setup has three parts: (1) a ServiceAccount with provider-specific annotations, (2) the `s3-backup-credentials` Secret without static keys, and (3) `spec.backup.serviceAccountName` on the instance.

**Example (AWS IRSA):**

1. Create an IRSA-annotated ServiceAccount in the instance namespace:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: openclaw-backup
  namespace: oc-tenant-my-tenant
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/openclaw-backup-role
```

2. Omit `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY` from the credentials Secret and set `S3_PROVIDER`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3-backup-credentials
  namespace: openclaw-operator-system
stringData:
  S3_ENDPOINT: "https://s3.us-east-1.amazonaws.com"
  S3_BUCKET: "my-openclaw-backups"
  S3_REGION: "us-east-1"
  S3_PROVIDER: "AWS"  # enables AWS-native credential chain
```

3. Reference the ServiceAccount in the instance spec:

```yaml
spec:
  backup:
    schedule: "0 2 * * *"
    serviceAccountName: "openclaw-backup"
```

When `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY` are omitted, the operator passes `--s3-env-auth=true` to rclone, which uses the provider's native credential chain. The `serviceAccountName` is set on all backup and restore Job pods so they inherit the cloud IAM role.

Setting `S3_PROVIDER` to the correct value (e.g., `AWS`, `GCS`) enables provider-specific optimizations in rclone. When left unset, it defaults to `Other` which works with any S3-compatible backend using static credentials.

## Pre-update backup deadline and transfer controls

`spec.autoUpdate.backupTimeout` (default `30m`, clamped to `5m`-`24h`) covers scale-down, pod termination, scheduling, and backup execution/retries. Its start time is persisted in `status.autoUpdate.backupStartedAt`. Kubernetes also enforces the Job execution budget if the operator is unavailable. On timeout or failure, `status.autoUpdate.updatePhase` becomes `AbortingBackup` while the operator stops the Job and waits for its Pods. It then restores the old version, records `lastUpdateError`, and waits for `checkInterval` before retrying. Cleanup and application startup may extend beyond the backup budget. The pre-delete `spec.backup.timeout` setting is unchanged.

To reduce the data transferred, configure `spec.backup.excludes` with [rclone glob patterns](https://rclone.org/filtering/) relative to the PVC root, for example `/tmp/plugin-captures/**`. The filters apply to pre-update, pre-delete, periodic uploads, and periodic snapshot copies. They do not filter restores or remove existing remote data. Excluded files are absent from new snapshots, so select only regenerable data.

`spec.backup.transfers` and `spec.backup.checkers` tune concurrency (defaults `8` and `16`; allowed range `1`-`256`). `spec.backup.fastList: true` reduces listing requests on supported backends at the cost of memory. Defaults preserve the current transfer behavior. Each pre-update backup still uses a fresh timestamped destination for its rollback snapshot; incremental pre-update uploads are not implemented.
