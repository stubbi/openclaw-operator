/*
Copyright 2026 Paperclip Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

const (
	updatePhaseBackingUp      = "BackingUp"
	updatePhaseAbortingBackup = "AbortingBackup"
	updatePhaseApplyingUpdate = "ApplyingUpdate"
	updatePhaseHealthCheck    = "HealthCheck"
	updatePhaseRollingBack    = "RollingBack"

	// maxRollbackCount is the circuit breaker threshold — auto-update pauses after this many consecutive rollbacks
	maxRollbackCount = int32(3)

	// defaultHealthCheckTimeout is used when the spec field is empty
	defaultHealthCheckTimeout = 10 * time.Minute
)

// resolveInitialTag resolves "latest" to a concrete semver tag when auto-update is enabled.
// Returns true if the tag was resolved (caller should requeue), false if no action was taken.
func (r *OpenClawInstanceReconciler) resolveInitialTag(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (bool, error) {
	logger := log.FromContext(ctx)

	if !isAutoUpdateEnabled(instance) {
		return false, nil
	}
	if instance.Spec.Image.Tag != "latest" {
		return false, nil
	}
	if r.VersionResolver == nil {
		return false, nil
	}

	repo := resources.GetImageRepository(instance)
	version, err := r.VersionResolver.LatestSemver(ctx, repo, nil)
	if err != nil {
		logger.Error(err, "Failed to resolve initial tag from registry, continuing with 'latest'")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdateResolveFailed",
			fmt.Sprintf("Failed to resolve latest version: %v", err))
		return false, nil // non-fatal
	}

	logger.Info("Resolved initial tag from registry", "tag", version)

	// Patch spec.image.tag
	original := instance.DeepCopy()
	instance.Spec.Image.Tag = version
	if err := r.Patch(ctx, instance, client.MergeFrom(original)); err != nil {
		return false, fmt.Errorf("patching image tag: %w", err)
	}

	// Update status
	instance.Status.AutoUpdate.CurrentVersion = version
	if err := r.Status().Update(ctx, instance); err != nil {
		return false, fmt.Errorf("updating auto-update status: %w", err)
	}

	r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateTagResolved",
		fmt.Sprintf("Resolved 'latest' to %s", version))

	return true, nil
}

// reconcileAutoUpdate handles the periodic version check and update state machine.
// It is called from Reconcile() after successful resource reconciliation.
func (r *OpenClawInstanceReconciler) reconcileAutoUpdate(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// If an update is in progress, drive the state machine
	if instance.Status.AutoUpdate.PendingVersion != "" {
		return r.driveUpdateStateMachine(ctx, instance)
	}

	// Guard: skip if auto-update is not enabled or applicable
	if !isAutoUpdateEnabled(instance) {
		return ctrl.Result{}, nil
	}

	if r.VersionResolver == nil {
		return ctrl.Result{}, nil
	}

	// Circuit breaker: pause version checks after too many consecutive rollbacks
	if instance.Status.AutoUpdate.RollbackCount >= maxRollbackCount {
		logger.Info("Auto-update paused due to repeated rollbacks", "rollbackCount", instance.Status.AutoUpdate.RollbackCount)
		return ctrl.Result{}, nil
	}

	// Check if it's time to check for updates
	if !shouldCheckForUpdate(instance) {
		return ctrl.Result{}, nil
	}

	// Perform version check
	repo := resources.GetImageRepository(instance)
	version, err := r.VersionResolver.LatestSemver(ctx, repo, nil)
	if err != nil {
		logger.Error(err, "Failed to check for updates")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdateCheckFailed",
			fmt.Sprintf("Failed to check for updates: %v", err))
		autoUpdateChecksTotal.WithLabelValues(instance.Name, instance.Namespace, "error").Inc()
		// Update last check time even on failure to avoid hammering the registry
		now := metav1.Now()
		instance.Status.AutoUpdate.LastCheckTime = &now
		return ctrl.Result{}, nil // non-fatal
	}

	now := metav1.Now()
	instance.Status.AutoUpdate.LastCheckTime = &now
	instance.Status.AutoUpdate.LatestVersion = version
	autoUpdateChecksTotal.WithLabelValues(instance.Name, instance.Namespace, "success").Inc()

	// Determine current version
	currentTag := instance.Spec.Image.Tag
	if currentTag == "" || currentTag == "latest" {
		// Existing instance still on "latest" — resolve to the concrete version now
		logger.Info("Resolving 'latest' tag to concrete version", "resolved", version)
		original := instance.DeepCopy()
		instance.Spec.Image.Tag = version
		if patchErr := r.Patch(ctx, instance, client.MergeFrom(original)); patchErr != nil {
			return ctrl.Result{}, fmt.Errorf("patching image tag from latest: %w", patchErr)
		}
		instance.Status.AutoUpdate.CurrentVersion = version
		r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateTagResolved",
			fmt.Sprintf("Resolved 'latest' to %s", version))
		return ctrl.Result{Requeue: true}, nil
	}

	currentVer, err := semver.NewVersion(currentTag)
	if err != nil {
		logger.V(1).Info("Current tag is not a valid semver, skipping update check", "tag", currentTag)
		return ctrl.Result{}, nil
	}

	latestVer, err := semver.NewVersion(version)
	if err != nil {
		logger.V(1).Info("Latest version from registry is not valid semver", "version", version)
		return ctrl.Result{}, nil
	}

	instance.Status.AutoUpdate.CurrentVersion = currentTag

	if !latestVer.GreaterThan(currentVer) {
		logger.V(1).Info("No new version available", "current", currentTag, "latest", version)
		// Clear any stale AutoUpdateAvailable condition
		meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateAvailable)
		return ctrl.Result{}, nil
	}

	// Clear stale FailedVersion if a newer version is now available
	if instance.Status.AutoUpdate.FailedVersion != "" {
		failedVer, fErr := semver.NewVersion(instance.Status.AutoUpdate.FailedVersion)
		if fErr == nil && latestVer.GreaterThan(failedVer) {
			logger.Info("Newer version available, clearing failed version", "failed", instance.Status.AutoUpdate.FailedVersion, "latest", version)
			instance.Status.AutoUpdate.FailedVersion = ""
		}
	}

	// Skip if the latest version is the same one that previously failed
	if instance.Status.AutoUpdate.FailedVersion == version {
		logger.Info("Skipping update to previously failed version", "version", version)
		return ctrl.Result{}, nil
	}

	// New version available — start update
	logger.Info("New version available, starting update", "current", currentTag, "latest", version)
	r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateAvailable",
		fmt.Sprintf("New version %s available (current: %s)", version, currentTag))

	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:    openclawv1alpha1.ConditionTypeAutoUpdateAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  "NewVersionAvailable",
		Message: fmt.Sprintf("Version %s is available (current: %s)", version, currentTag),
	})

	instance.Status.AutoUpdate.PendingVersion = version
	instance.Status.AutoUpdate.UpdatePhase = ""
	instance.Status.AutoUpdate.BackupStartedAt = nil
	instance.Status.AutoUpdate.LastUpdateError = ""

	return ctrl.Result{Requeue: true}, nil
}

// driveUpdateStateMachine manages the multi-step update process.
//
// State machine:
//  1. Set phase = Updating
//  2. If backupBeforeUpdate && persistence enabled:
//     a. Scale StatefulSet to 0 → requeue
//     b. Wait for pods to terminate → requeue
//     c. Create backup job → requeue
//     d. Poll job → requeue
//     e. On failure: abort update
//  3. Save previousVersion, preUpdateBackupPath
//  4. Patch spec.image.tag = pendingVersion
//  5. Set UpdatePhase = HealthCheck, keep PendingVersion set
//  6. Health check loop: wait for StatefulSet readiness or timeout
//  7. On timeout (if rollbackOnFailure): rollback
func (r *OpenClawInstanceReconciler) driveUpdateStateMachine(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pendingVersion := instance.Status.AutoUpdate.PendingVersion

	// Resume cleanup after errors or controller restarts before allowing any
	// resource reconciliation to restart the gateway against an active backup.
	if instance.Status.AutoUpdate.UpdatePhase == updatePhaseAbortingBackup {
		return r.abortPreUpdateBackup(ctx, instance, instance.Status.AutoUpdate.LastUpdateError)
	}

	// Handle in-progress health check
	if instance.Status.AutoUpdate.UpdatePhase == updatePhaseHealthCheck {
		return r.driveHealthCheck(ctx, instance)
	}

	// Handle in-progress rollback
	if instance.Status.AutoUpdate.UpdatePhase == updatePhaseRollingBack {
		return r.driveRollback(ctx, instance)
	}

	// Step 1: Set phase to Updating
	if instance.Status.Phase != openclawv1alpha1.PhaseUpdating {
		instance.Status.Phase = openclawv1alpha1.PhaseUpdating
		updatePhaseMetric(instance.Name, instance.Namespace, instance.Status.Phase)
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Step 2: Backup if needed
	needsBackup := instance.Spec.AutoUpdate.BackupBeforeUpdate == nil || *instance.Spec.AutoUpdate.BackupBeforeUpdate

	if needsBackup && resources.IsPersistenceEnabled(instance) {
		switch instance.Status.AutoUpdate.UpdatePhase {
		case "", updatePhaseBackingUp:
			result, done, err := r.drivePreUpdateBackup(ctx, instance)
			if err != nil {
				return r.abortPreUpdateBackup(ctx, instance, fmt.Sprintf("backup failed: %v", err))
			}
			if !done {
				// Still in progress
				instance.Status.AutoUpdate.UpdatePhase = updatePhaseBackingUp
				if statusErr := r.Status().Update(ctx, instance); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return result, nil
			}
			// Backup complete — save the backup path for potential rollback restore
			instance.Status.AutoUpdate.PreUpdateBackupPath = r.lastPreUpdateBackupPath(ctx, instance)
			instance.Status.AutoUpdate.BackupStartedAt = nil
		}
	}

	// Keep backup failures visible across recovery and retries. Only clear the
	// condition once an attempt passes (or explicitly skips) the backup stage.
	meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateBlocked)

	// Step 3: Save previous version for rollback
	instance.Status.AutoUpdate.PreviousVersion = instance.Spec.Image.Tag

	// Step 4: Apply the update — patch spec.image.tag
	instance.Status.AutoUpdate.UpdatePhase = updatePhaseApplyingUpdate
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Applying auto-update", "from", instance.Spec.Image.Tag, "to", pendingVersion)

	original := instance.DeepCopy()
	instance.Spec.Image.Tag = pendingVersion
	if err := r.Patch(ctx, instance, client.MergeFrom(original)); err != nil {
		return r.abortUpdate(ctx, instance, fmt.Sprintf("failed to patch image tag: %v", err))
	}

	// Step 4b: Scale StatefulSet back up if it was scaled down for backup.
	// The backup flow scales to 0 replicas, and the main reconcile loop won't
	// reach reconcileResources() while pendingVersion is set (early return),
	// so we must explicitly restore replicas here (#299).
	if needsBackup && resources.IsPersistenceEnabled(instance) {
		sts := &appsv1.StatefulSet{}
		stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
		if getErr := r.Get(ctx, stsKey, sts); getErr == nil {
			if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
				one := int32(1)
				stsOriginal := sts.DeepCopy()
				sts.Spec.Replicas = &one
				if patchErr := r.Patch(ctx, sts, client.MergeFrom(stsOriginal)); patchErr != nil {
					return r.abortUpdate(ctx, instance, fmt.Sprintf("failed to scale StatefulSet back up: %v", patchErr))
				}
				logger.Info("Scaled StatefulSet back up after pre-update backup")
			}
		}
	}

	// Step 5: Enter health check phase (keep PendingVersion set)
	rollbackEnabled := instance.Spec.AutoUpdate.RollbackOnFailure == nil || *instance.Spec.AutoUpdate.RollbackOnFailure
	if rollbackEnabled {
		now := metav1.Now()
		instance.Status.AutoUpdate.UpdatePhase = updatePhaseHealthCheck
		instance.Status.AutoUpdate.LastUpdateTime = &now
		instance.Status.Phase = openclawv1alpha1.PhaseProvisioning
		// PendingVersion stays set — signals "update not yet confirmed"

		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}

		autoUpdateAppliedTotal.WithLabelValues(instance.Name, instance.Namespace).Inc()
		r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateApplied",
			fmt.Sprintf("Updated image tag to %s, health check started", pendingVersion))

		logger.Info("Auto-update applied, entering health check", "version", pendingVersion)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Rollback disabled — immediately confirm the update (legacy behavior)
	now := metav1.Now()
	instance.Status.AutoUpdate.CurrentVersion = pendingVersion
	instance.Status.AutoUpdate.PendingVersion = ""
	instance.Status.AutoUpdate.UpdatePhase = ""
	instance.Status.AutoUpdate.LastUpdateTime = &now
	instance.Status.AutoUpdate.LastUpdateError = ""
	instance.Status.AutoUpdate.PreviousVersion = ""
	instance.Status.AutoUpdate.PreUpdateBackupPath = ""
	instance.Status.AutoUpdate.RollbackCount = 0
	instance.Status.Phase = openclawv1alpha1.PhaseProvisioning

	meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateAvailable)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	autoUpdateAppliedTotal.WithLabelValues(instance.Name, instance.Namespace).Inc()
	r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateApplied",
		fmt.Sprintf("Updated image tag to %s", pendingVersion))

	logger.Info("Auto-update applied successfully", "version", pendingVersion)
	return ctrl.Result{Requeue: true}, nil
}

// driveHealthCheck monitors the StatefulSet after an update and either confirms
// the update or triggers a rollback if the health check timeout elapses.
func (r *OpenClawInstanceReconciler) driveHealthCheck(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check StatefulSet readiness
	sts := &appsv1.StatefulSet{}
	stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
	if err := r.Get(ctx, stsKey, sts); err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet doesn't exist yet — requeue
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Success: pod is ready and updated
	if sts.Status.ReadyReplicas > 0 && sts.Status.UpdatedReplicas > 0 {
		logger.Info("Health check passed, update confirmed", "version", instance.Status.AutoUpdate.PendingVersion)

		instance.Status.AutoUpdate.CurrentVersion = instance.Status.AutoUpdate.PendingVersion
		instance.Status.AutoUpdate.PendingVersion = ""
		instance.Status.AutoUpdate.UpdatePhase = ""
		instance.Status.AutoUpdate.LastUpdateError = ""
		instance.Status.AutoUpdate.PreviousVersion = ""
		instance.Status.AutoUpdate.PreUpdateBackupPath = ""
		instance.Status.AutoUpdate.RollbackCount = 0
		instance.Status.Phase = openclawv1alpha1.PhaseRunning

		meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateAvailable)

		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}

		r.Recorder.Event(instance, corev1.EventTypeNormal, "AutoUpdateConfirmed",
			fmt.Sprintf("Update to %s confirmed — pod is healthy", instance.Status.AutoUpdate.CurrentVersion))

		return ctrl.Result{Requeue: true}, nil
	}

	// Check timeout
	timeout := parseHealthCheckTimeout(instance.Spec.AutoUpdate.HealthCheckTimeout)
	if instance.Status.AutoUpdate.LastUpdateTime == nil {
		// Shouldn't happen, but handle gracefully
		now := metav1.Now()
		instance.Status.AutoUpdate.LastUpdateTime = &now
	}
	elapsed := time.Since(instance.Status.AutoUpdate.LastUpdateTime.Time)

	if elapsed < timeout {
		logger.V(1).Info("Health check in progress", "elapsed", elapsed.Round(time.Second), "timeout", timeout)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Timeout elapsed — trigger rollback
	logger.Info("Health check timeout elapsed, triggering rollback",
		"pendingVersion", instance.Status.AutoUpdate.PendingVersion,
		"previousVersion", instance.Status.AutoUpdate.PreviousVersion,
		"elapsed", elapsed.Round(time.Second))

	instance.Status.AutoUpdate.UpdatePhase = updatePhaseRollingBack
	instance.Status.Phase = openclawv1alpha1.PhaseUpdating
	updatePhaseMetric(instance.Name, instance.Namespace, instance.Status.Phase)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdateHealthCheckFailed",
		fmt.Sprintf("Pod failed to become ready within %s, rolling back from %s to %s",
			timeout, instance.Status.AutoUpdate.PendingVersion, instance.Status.AutoUpdate.PreviousVersion))

	return ctrl.Result{Requeue: true}, nil
}

// driveRollback reverts the image tag to the previous version and optionally
// restores the PVC from the pre-update backup.
func (r *OpenClawInstanceReconciler) driveRollback(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	previousVersion := instance.Status.AutoUpdate.PreviousVersion
	if previousVersion == "" {
		// Can't rollback without knowing the previous version
		return r.abortUpdate(ctx, instance, "rollback failed: no previous version recorded")
	}

	backupPath := instance.Status.AutoUpdate.PreUpdateBackupPath
	// Step 8b: Restore from backup if available and persistence is enabled
	if backupPath != "" && resources.IsPersistenceEnabled(instance) {
		result, done, err := r.driveRollbackRestore(ctx, instance, backupPath)
		if err != nil {
			logger.Error(err, "Rollback restore failed, reverting image tag only")
			r.Recorder.Event(instance, corev1.EventTypeWarning, "RollbackRestoreFailed",
				fmt.Sprintf("Failed to restore from %s: %v — reverting image tag only", backupPath, err))
			// Fall through to revert image tag even if restore fails
		} else if !done {
			return result, nil
		}
	}

	// Step 8c: Revert image tag
	logger.Info("Rolling back image tag", "from", instance.Spec.Image.Tag, "to", previousVersion)

	original := instance.DeepCopy()
	instance.Spec.Image.Tag = previousVersion
	if err := r.Patch(ctx, instance, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("rollback patch failed: %w", err)
	}

	// Step 8d-g: Update status
	failedVersion := instance.Status.AutoUpdate.PendingVersion
	instance.Status.AutoUpdate.FailedVersion = failedVersion
	instance.Status.AutoUpdate.PendingVersion = ""
	instance.Status.AutoUpdate.UpdatePhase = ""
	instance.Status.AutoUpdate.PreUpdateBackupPath = ""
	instance.Status.AutoUpdate.PreviousVersion = ""
	instance.Status.AutoUpdate.LastUpdateError = fmt.Sprintf("version %s failed health check, rolled back to %s", failedVersion, previousVersion)
	instance.Status.AutoUpdate.RollbackCount++
	instance.Status.AutoUpdate.CurrentVersion = previousVersion
	instance.Status.Phase = openclawv1alpha1.PhaseProvisioning

	meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateAvailable)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	autoUpdateRollbacksTotal.WithLabelValues(instance.Name, instance.Namespace).Inc()

	// Step 8h: Emit warning event
	r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdateRolledBack",
		fmt.Sprintf("Rolled back from %s to %s (rollback count: %d/%d)",
			failedVersion, previousVersion, instance.Status.AutoUpdate.RollbackCount, maxRollbackCount))

	if instance.Status.AutoUpdate.RollbackCount >= maxRollbackCount {
		r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdatePaused",
			fmt.Sprintf("Auto-update paused after %d consecutive rollbacks", instance.Status.AutoUpdate.RollbackCount))
	}

	logger.Info("Auto-update rolled back", "failedVersion", failedVersion, "restoredVersion", previousVersion,
		"rollbackCount", instance.Status.AutoUpdate.RollbackCount)

	return ctrl.Result{Requeue: true}, nil
}

// driveRollbackRestore handles the PVC restore during rollback.
// Returns (result, done, err) where done=true means restore completed.
func (r *OpenClawInstanceReconciler) driveRollbackRestore(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance, backupPath string) (requeueResult ctrl.Result, done bool, retErr error) {
	logger := log.FromContext(ctx)

	// Scale down StatefulSet
	sts := &appsv1.StatefulSet{}
	stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
	if err := r.Get(ctx, stsKey, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, nil
		}
		return ctrl.Result{}, false, err
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0 {
		logger.Info("Scaling down StatefulSet for rollback restore")
		zero := int32(0)
		original := sts.DeepCopy()
		sts.Spec.Replicas = &zero
		if err := r.Patch(ctx, sts, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
	}

	// Wait for pods to terminate
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(resources.SelectorLabels(instance)),
	); err != nil {
		return ctrl.Result{}, false, err
	}
	if len(podList.Items) > 0 {
		logger.Info("Waiting for pods to terminate for rollback restore", "count", len(podList.Items))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
	}

	// Get S3 credentials
	creds, err := r.getS3Credentials(ctx)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("failed to get S3 credentials for rollback: %w", err)
	}

	// Reconcile mirror Secret for secretKeyRef (no-op for env-auth mode)
	if mirrorErr := r.reconcileS3MirrorSecret(ctx, instance, creds); mirrorErr != nil {
		return ctrl.Result{}, false, mirrorErr
	}

	// Create or check restore job
	jobName := rollbackRestoreJobName(instance)
	existingJob, err := r.getJob(ctx, jobName, instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}

	if apierrors.IsNotFound(err) || existingJob == nil {
		pvcName := pvcNameForInstance(instance)
		labels := backupLabels(instance, "rollback-restore")

		job := buildRcloneJob(jobName, instance.Namespace, pvcName, backupPath, labels, creds, false, instance.Spec.Availability.NodeSelector, instance.Spec.Availability.Tolerations, instance.Spec.Backup.ServiceAccountName, mirrorSecretName(instance), &instance.Spec.Backup)
		if err := controllerutil.SetControllerReference(instance, job, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}

		logger.Info("Creating rollback restore job", "job", jobName, "remotePath", backupPath)
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
			}
			return ctrl.Result{}, false, err
		}
		r.Recorder.Event(instance, corev1.EventTypeNormal, "RollbackRestoreStarted",
			fmt.Sprintf("Rollback restore job %s created from %s", jobName, backupPath))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
	}

	// Job exists — check status
	finished, condType := isJobFinished(existingJob)
	if !finished {
		logger.Info("Rollback restore job still running", "job", jobName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
	}

	if condType == batchv1.JobFailed {
		return ctrl.Result{}, false, fmt.Errorf("rollback restore job %s failed", jobName)
	}

	logger.Info("Rollback restore completed successfully", "job", jobName)
	r.Recorder.Event(instance, corev1.EventTypeNormal, "RollbackRestoreComplete",
		"Rollback restore completed successfully")

	return ctrl.Result{}, true, nil
}

// rollbackRestoreJobName returns a deterministic name for the rollback restore Job.
func rollbackRestoreJobName(instance *openclawv1alpha1.OpenClawInstance) string {
	return instance.Name + "-rollback-restore"
}

// lastPreUpdateBackupPath extracts the completed backup destination for rollback.
func (r *OpenClawInstanceReconciler) lastPreUpdateBackupPath(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) string {
	job, err := r.getJob(ctx, preUpdateBackupJobName(instance), instance.Namespace)
	if err != nil || job == nil {
		return ""
	}
	creds, err := r.getS3Credentials(ctx)
	if err != nil {
		return ""
	}
	prefix := fmt.Sprintf(":s3:%s/", creds.Bucket)
	for i := range job.Spec.Template.Spec.Containers {
		c := &job.Spec.Template.Spec.Containers[i]
		// Backup args are sync, /data/, destination, flags.
		if c.Name == "rclone" && len(c.Args) >= 3 && strings.HasPrefix(c.Args[2], prefix) {
			return strings.TrimPrefix(c.Args[2], prefix)
		}
	}
	return ""
}

// parseHealthCheckTimeout parses the health check timeout string with min/max bounds.
func parseHealthCheckTimeout(s string) time.Duration {
	if s == "" {
		return defaultHealthCheckTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultHealthCheckTimeout
	}
	if d < 2*time.Minute {
		return 2 * time.Minute
	}
	if d > 30*time.Minute {
		return 30 * time.Minute
	}
	return d
}

// drivePreUpdateBackup handles the backup steps before applying an update.
// Returns (result, done, err) where done=true means backup completed successfully.
func (r *OpenClawInstanceReconciler) drivePreUpdateBackup(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (requeueResult ctrl.Result, done bool, retErr error) {
	logger := log.FromContext(ctx)

	// Persist the start before scaling down so pod termination and scheduling
	// count toward the budget, and a controller restart cannot reset it.
	jobName := preUpdateBackupJobName(instance)
	existingJob, err := r.getJob(ctx, jobName, instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}
	if instance.Status.AutoUpdate.BackupStartedAt == nil {
		started := metav1.NewTime(time.Now().Truncate(time.Second))
		// Adopt an in-flight Job from an older operator without granting it a
		// fresh timeout. A new update instead retires any prior attempt below.
		if instance.Status.AutoUpdate.UpdatePhase == updatePhaseBackingUp && existingJob != nil && !existingJob.CreationTimestamp.IsZero() {
			started = existingJob.CreationTimestamp
		}
		instance.Status.AutoUpdate.BackupStartedAt = &started
		if statusErr := r.Status().Update(ctx, instance); statusErr != nil {
			return ctrl.Result{}, false, statusErr
		}
		return ctrl.Result{Requeue: true}, false, nil
	}
	started := instance.Status.AutoUpdate.BackupStartedAt.Time
	timeout := parseBackupTimeout(instance.Spec.AutoUpdate.BackupTimeout)
	remaining := timeout - time.Since(started)
	if existingJob != nil && existingJob.CreationTimestamp.Time.Before(started) {
		// Jobs have a deterministic name and may outlive a successful update.
		// Never reuse an earlier update's snapshot or failed Job.
		stopped, stopErr := r.stopPreUpdateBackup(ctx, instance)
		if stopErr != nil {
			return ctrl.Result{}, false, stopErr
		}
		if !stopped {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
		}
		existingJob = nil
	}
	if existingJob != nil {
		finished, condition := isJobFinished(existingJob)
		if finished && condition == batchv1.JobComplete {
			// A completed Job may be observed after the deadline on a delayed
			// reconcile. Its Kubernetes deadline already bounded execution.
			r.Recorder.Event(instance, corev1.EventTypeNormal, "PreUpdateBackupComplete", "Pre-update backup completed successfully")
			return ctrl.Result{}, true, nil
		}
		if finished && condition == batchv1.JobFailed {
			for _, jobCondition := range existingJob.Status.Conditions {
				if jobCondition.Type == batchv1.JobFailed && jobCondition.Status == corev1.ConditionTrue {
					return ctrl.Result{}, false, fmt.Errorf("pre-update backup job %s failed: %s: %s", jobName, jobCondition.Reason, jobCondition.Message)
				}
			}
			return ctrl.Result{}, false, fmt.Errorf("pre-update backup job %s failed", jobName)
		}
	}
	if remaining <= 0 {
		return ctrl.Result{}, false, fmt.Errorf("pre-update backup timed out after %s", timeout)
	}

	// Scale down StatefulSet
	sts := &appsv1.StatefulSet{}
	stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
	if getErr := r.Get(ctx, stsKey, sts); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// No StatefulSet, skip directly to applying update
			return ctrl.Result{}, true, nil
		}
		return ctrl.Result{}, false, getErr
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0 {
		logger.Info("Scaling down StatefulSet for pre-update backup")
		zero := int32(0)
		original := sts.DeepCopy()
		sts.Spec.Replicas = &zero
		if patchErr := r.Patch(ctx, sts, client.MergeFrom(original)); patchErr != nil {
			return ctrl.Result{}, false, patchErr
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
	}

	// Wait for pods to terminate
	podList := &corev1.PodList{}
	if listErr := r.List(ctx, podList,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(resources.SelectorLabels(instance)),
	); listErr != nil {
		return ctrl.Result{}, false, listErr
	}
	if len(podList.Items) > 0 {
		logger.Info("Waiting for pods to terminate for pre-update backup", "count", len(podList.Items))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
	}

	// Get S3 credentials
	creds, err := r.getS3Credentials(ctx)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("failed to get S3 credentials: %w", err)
	}

	// Reconcile mirror Secret for secretKeyRef (no-op for env-auth mode)
	if mirrorErr := r.reconcileS3MirrorSecret(ctx, instance, creds); mirrorErr != nil {
		return ctrl.Result{}, false, mirrorErr
	}

	// Create or check backup job
	if existingJob == nil {
		// Create the backup job
		tenantID := getTenantID(instance)
		timestamp := time.Now().UTC().Format("2006-01-02T150405Z")
		b2Path := fmt.Sprintf("backups/%s/%s/pre-update-%s", tenantID, instance.Name, timestamp)
		pvcName := pvcNameForInstance(instance)
		labels := backupLabels(instance, "pre-update-backup")

		job := buildRcloneJob(jobName, instance.Namespace, pvcName, b2Path, labels, creds, true, instance.Spec.Availability.NodeSelector, instance.Spec.Availability.Tolerations, instance.Spec.Backup.ServiceAccountName, mirrorSecretName(instance), &instance.Spec.Backup)
		// Bound retries and stuck transfers even while the operator is down.
		deadline := max(int64(1), int64(remaining.Seconds()))
		job.Spec.ActiveDeadlineSeconds = &deadline
		if err := controllerutil.SetControllerReference(instance, job, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}

		logger.Info("Creating pre-update backup job", "job", jobName, "remotePath", b2Path)
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
			}
			return ctrl.Result{}, false, err
		}
		r.Recorder.Event(instance, corev1.EventTypeNormal, "PreUpdateBackupStarted",
			fmt.Sprintf("Pre-update backup job %s created", jobName))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
	}

	logger.Info("Pre-update backup job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
}

// stopPreUpdateBackup waits for foreground deletion, then checks Pods as well.
// The gateway must not restart while rclone still holds/reads the PVC.
func (r *OpenClawInstanceReconciler) stopPreUpdateBackup(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (bool, error) {
	job, err := r.getJob(ctx, preUpdateBackupJobName(instance), instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if job != nil {
		if job.DeletionTimestamp.IsZero() {
			if deleteErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				return false, deleteErr
			}
		}
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabels(backupLabels(instance, "pre-update-backup"))); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

func (r *OpenClawInstanceReconciler) abortPreUpdateBackup(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance, reason string) (ctrl.Result, error) {
	retry := ctrl.Result{RequeueAfter: 5 * time.Second}
	if instance.Status.AutoUpdate.UpdatePhase != updatePhaseAbortingBackup {
		instance.Status.AutoUpdate.UpdatePhase = updatePhaseAbortingBackup
		instance.Status.AutoUpdate.LastUpdateError = reason
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               openclawv1alpha1.ConditionTypeAutoUpdateBlocked,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: instance.Generation,
			Reason:             "PreUpdateBackupFailed",
			Message:            reason,
		})
		if err := r.Status().Update(ctx, instance); err != nil {
			return retry, err
		}
	}
	stopped, err := r.stopPreUpdateBackup(ctx, instance)
	if err != nil || !stopped {
		return retry, err
	}
	return r.abortUpdate(ctx, instance, reason)
}

// abortUpdate clears the update state and returns to Running phase.
func (r *OpenClawInstanceReconciler) abortUpdate(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(nil, "Aborting auto-update", "reason", reason)

	// Keep the update pending until scale-up succeeds, so transient API errors
	// are retried through the same cleanup path.
	sts := &appsv1.StatefulSet{}
	stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
	if err := r.Get(ctx, stsKey, sts); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	} else if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
		original := sts.DeepCopy()
		sts.Spec.Replicas = resources.Ptr(int32(1))
		if err := r.Patch(ctx, sts, client.MergeFrom(original)); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	}
	instance.Status.AutoUpdate.PendingVersion = ""
	instance.Status.AutoUpdate.UpdatePhase = ""
	instance.Status.AutoUpdate.BackupStartedAt = nil
	instance.Status.AutoUpdate.PreUpdateBackupPath = ""
	instance.Status.AutoUpdate.LastUpdateError = reason
	// A long backup may have consumed the check interval. Avoid immediately
	// taking the instance down again to retry the same backup.
	now := metav1.Now()
	instance.Status.AutoUpdate.LastCheckTime = &now
	instance.Status.Phase = openclawv1alpha1.PhaseRunning
	meta.RemoveStatusCondition(&instance.Status.Conditions, openclawv1alpha1.ConditionTypeAutoUpdateAvailable)
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	r.Recorder.Event(instance, corev1.EventTypeWarning, "AutoUpdateAborted", reason)

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// preUpdateBackupJobName returns a deterministic name for the pre-update backup Job.
func preUpdateBackupJobName(instance *openclawv1alpha1.OpenClawInstance) string {
	return instance.Name + "-pre-update-backup"
}

// isAutoUpdateEnabled returns true if auto-update is enabled and no digest pin is set.
func isAutoUpdateEnabled(instance *openclawv1alpha1.OpenClawInstance) bool {
	if instance.Spec.AutoUpdate.Enabled == nil || !*instance.Spec.AutoUpdate.Enabled {
		return false
	}
	// Digest pin overrides auto-update
	if instance.Spec.Image.Digest != "" {
		return false
	}
	return true
}

// shouldCheckForUpdate returns true if enough time has elapsed since the last check.
func shouldCheckForUpdate(instance *openclawv1alpha1.OpenClawInstance) bool {
	if instance.Status.AutoUpdate.LastCheckTime == nil {
		return true
	}
	interval := parseCheckInterval(instance.Spec.AutoUpdate.CheckInterval)
	return time.Since(instance.Status.AutoUpdate.LastCheckTime.Time) >= interval
}

// parseCheckInterval parses the check interval string with min/max bounds.
func parseCheckInterval(s string) time.Duration {
	if s == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 24 * time.Hour
	}
	if d < time.Hour {
		return time.Hour
	}
	if d > 168*time.Hour {
		return 168 * time.Hour
	}
	return d
}
