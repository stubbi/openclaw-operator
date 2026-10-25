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
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	// defaultBackupTimeout is the maximum time the operator waits for a pre-delete
	// backup to complete before giving up and proceeding with deletion.
	defaultBackupTimeout = 30 * time.Minute
)

// reconcileDeleteWithBackup implements the backup-before-delete state machine:
//  1. Check skip-backup annotation -> if set, remove finalizer immediately
//  2. Check backup timeout -> if elapsed, skip backup and proceed with deletion
//  3. Scale down StatefulSet to 0 -> requeue 5s
//  4. Wait for pods to terminate -> requeue 5s
//  5. Create/check backup Job
//  6. On success: remove finalizer -> K8s GCs all owned resources
func (r *OpenClawInstanceReconciler) reconcileDeleteWithBackup(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion with backup", "instance", instance.Name, "namespace", instance.Namespace)

	// Step 0: Check skip-backup annotation — before any phase change or scale-down
	if instance.Annotations[AnnotationSkipBackup] == "true" {
		logger.Info("Skip-backup annotation set, removing finalizer immediately")
		instance.Status.Phase = openclawv1alpha1.PhaseTerminating
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return r.removeFinalizer(ctx, instance)
	}

	// Check if persistence is enabled — no PVC means nothing to back up
	if !resources.IsPersistenceEnabled(instance) {
		logger.Info("Persistence disabled, skipping backup")
		instance.Status.Phase = openclawv1alpha1.PhaseTerminating
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return r.removeFinalizer(ctx, instance)
	}

	// Step 1: Update phase to BackingUp and record start time (if not already terminating/backing up)
	if instance.Status.Phase != openclawv1alpha1.PhaseBackingUp && instance.Status.Phase != openclawv1alpha1.PhaseTerminating {
		now := metav1.Now()
		instance.Status.Phase = openclawv1alpha1.PhaseBackingUp
		instance.Status.BackingUpSince = &now
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Step 2: Check backup timeout
	if instance.Status.BackingUpSince != nil {
		timeout := parseBackupTimeout(instance.Spec.Backup.Timeout)
		elapsed := time.Since(instance.Status.BackingUpSince.Time)
		if elapsed >= timeout {
			logger.Info("Backup timeout exceeded, skipping backup and proceeding with deletion",
				"elapsed", elapsed.Round(time.Second), "timeout", timeout)
			r.Recorder.Event(instance, corev1.EventTypeWarning, "BackupTimedOut",
				fmt.Sprintf("Backup did not complete within %s - skipping backup and proceeding with deletion", timeout))

			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:    openclawv1alpha1.ConditionTypeBackupComplete,
				Status:  metav1.ConditionFalse,
				Reason:  "BackupTimedOut",
				Message: fmt.Sprintf("Backup did not complete within %s", timeout),
			})
			instance.Status.Phase = openclawv1alpha1.PhaseTerminating
			instance.Status.BackingUpSince = nil
			if err := r.Status().Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
			return r.removeFinalizer(ctx, instance)
		}
	}

	// Step 3: Scale down StatefulSet to 0
	sts := &appsv1.StatefulSet{}
	stsKey := client.ObjectKey{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}
	if err := r.Get(ctx, stsKey, sts); err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet doesn't exist — no pods to worry about, proceed to backup
			logger.Info("StatefulSet not found, proceeding to backup")
		} else {
			return ctrl.Result{}, err
		}
	} else {
		// Scale to 0 if needed
		if sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0 {
			logger.Info("Scaling down StatefulSet to 0")
			zero := int32(0)
			sts.Spec.Replicas = &zero
			if err := r.Update(ctx, sts); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Step 4: Wait for pods to terminate
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(instance.Namespace),
			client.MatchingLabels(resources.SelectorLabels(instance)),
		); err != nil {
			return ctrl.Result{}, err
		}
		if len(podList.Items) > 0 {
			logger.Info("Waiting for pods to terminate", "count", len(podList.Items))
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Step 5: Create/check backup Job
	creds, err := r.getS3Credentials(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// S3 credentials not configured - skip backup gracefully
			logger.Info("S3 backup credentials not configured, skipping backup")
			r.Recorder.Event(instance, corev1.EventTypeNormal, "BackupSkipped",
				"S3 backup credentials Secret not found - skipping pre-delete backup")
			instance.Status.Phase = openclawv1alpha1.PhaseTerminating
			if statusErr := r.Status().Update(ctx, instance); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return r.removeFinalizer(ctx, instance)
		}
		// Other errors (RBAC, network) - retry with warning
		logger.Error(err, "Failed to get S3 credentials, cannot backup")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "BackupCredentialsFailed", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Reconcile mirror Secret for secretKeyRef (no-op for env-auth mode)
	if mirrorErr := r.reconcileS3MirrorSecret(ctx, instance, creds); mirrorErr != nil {
		return ctrl.Result{}, mirrorErr
	}

	tenantID := getTenantID(instance)
	timestamp := time.Now().UTC().Format("2006-01-02T150405Z")
	b2Path := fmt.Sprintf("backups/%s/%s/%s", tenantID, instance.Name, timestamp)
	jobName := backupJobName(instance)

	// Check for existing Job
	existingJob, err := r.getJob(ctx, jobName, instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) || existingJob == nil {
		// Create backup Job
		// Use the path from status if already set (idempotent on requeue)
		if instance.Status.LastBackupPath != "" {
			b2Path = instance.Status.LastBackupPath
		} else {
			instance.Status.LastBackupPath = b2Path
			instance.Status.BackupJobName = jobName
			if err := r.Status().Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}

		pvcName := pvcNameForInstance(instance)
		labels := backupLabels(instance, "backup")
		job := buildRcloneJob(jobName, instance.Namespace, pvcName, b2Path, labels, creds, true, instance.Spec.Availability.NodeSelector, instance.Spec.Availability.Tolerations, instance.Spec.Backup.ServiceAccountName, mirrorSecretName(instance), &instance.Spec.Backup)

		// Set owner reference so the Job is cleaned up with the instance
		if err := controllerutil.SetControllerReference(instance, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Creating backup Job", "job", jobName, "remotePath", b2Path)
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Race condition — Job was created between our check and create
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		r.Recorder.Event(instance, corev1.EventTypeNormal, "BackupStarted", fmt.Sprintf("Backup Job %s created, target: %s", jobName, b2Path))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Job exists — check its status
	finished, condType := isJobFinished(existingJob)
	if !finished {
		logger.Info("Backup Job still running", "job", jobName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if condType == batchv1.JobFailed {
		logger.Error(nil, "Backup Job failed - retrying until timeout", "job", jobName)
		r.Recorder.Event(instance, corev1.EventTypeWarning, "BackupFailed",
			fmt.Sprintf("Backup Job %s failed. Will retry until backup timeout elapses. To skip immediately: annotate %s=true.", jobName, AnnotationSkipBackup))

		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:    openclawv1alpha1.ConditionTypeBackupComplete,
			Status:  metav1.ConditionFalse,
			Reason:  "BackupFailed",
			Message: "Backup Job failed",
		})
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 6: Backup succeeded - record and remove finalizer
	logger.Info("Backup Job completed successfully", "job", jobName, "remotePath", instance.Status.LastBackupPath)
	r.Recorder.Event(instance, corev1.EventTypeNormal, "BackupComplete",
		fmt.Sprintf("Backup completed to %s", instance.Status.LastBackupPath))

	now := metav1.Now()
	instance.Status.LastBackupTime = &now
	instance.Status.Phase = openclawv1alpha1.PhaseTerminating
	instance.Status.BackingUpSince = nil

	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:    openclawv1alpha1.ConditionTypeBackupComplete,
		Status:  metav1.ConditionTrue,
		Reason:  "BackupSucceeded",
		Message: fmt.Sprintf("Backup completed to %s", instance.Status.LastBackupPath),
	})
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	return r.removeFinalizer(ctx, instance)
}

// parseBackupTimeout parses the backup timeout string with min/max bounds.
// Returns defaultBackupTimeout (30m) when empty. Minimum: 5m, Maximum: 24h.
func parseBackupTimeout(s string) time.Duration {
	if s == "" {
		return defaultBackupTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultBackupTimeout
	}
	if d < 5*time.Minute {
		return 5 * time.Minute
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// removeFinalizer removes the operator finalizer, allowing K8s to GC the resource.
// When spec.storage.persistence.orphan is true (the default), the PVC owner reference
// is removed first so K8s does not garbage-collect the PVC with the CR.
func (r *OpenClawInstanceReconciler) removeFinalizer(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Orphan the PVC unless the user explicitly set orphan=false.
	// Only applies to operator-managed PVCs (not existingClaim).
	orphan := instance.Spec.Storage.Persistence.Orphan == nil || *instance.Spec.Storage.Persistence.Orphan
	usingExistingClaim := instance.Spec.Storage.Persistence.ExistingClaim != ""
	if orphan && resources.IsPersistenceEnabled(instance) && !usingExistingClaim {
		if err := r.orphanPVC(ctx, instance); err != nil {
			logger.Error(err, "Failed to orphan PVC - proceeding with finalizer removal")
		}
	}

	controllerutil.RemoveFinalizer(instance, FinalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up per-instance metrics to avoid stale entries
	instanceReady.DeleteLabelValues(instance.Name, instance.Namespace)
	instanceInfo.DeletePartialMatch(prometheus.Labels{
		"instance":  instance.Name,
		"namespace": instance.Namespace,
	})

	logger.Info("Finalizer removed, cleanup complete")
	return ctrl.Result{}, nil
}

// orphanPVC removes the owner reference pointing to instance from the managed PVC
// so that Kubernetes does not garbage-collect it when the CR is deleted.
func (r *OpenClawInstanceReconciler) orphanPVC(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) error {
	logger := log.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := client.ObjectKey{Name: resources.PVCName(instance), Namespace: instance.Namespace}
	if err := r.Get(ctx, pvcKey, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // nothing to orphan
		}
		return fmt.Errorf("failed to get PVC %s: %w", pvcKey.Name, err)
	}

	// Filter out any owner reference that points to this instance
	refs := pvc.OwnerReferences[:0]
	for _, ref := range pvc.OwnerReferences {
		if ref.UID != instance.UID {
			refs = append(refs, ref)
		}
	}
	if len(refs) == len(pvc.OwnerReferences) {
		return nil // owner reference already absent
	}

	pvc.OwnerReferences = refs
	if err := r.Update(ctx, pvc); err != nil {
		return fmt.Errorf("failed to remove owner reference from PVC %s: %w", pvcKey.Name, err)
	}
	logger.Info("PVC orphaned - owner reference removed, PVC will be retained after CR deletion", "pvc", pvcKey.Name)
	return nil
}
