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
)

// reconcileRestore handles restoring PVC data from an S3 backup before StatefulSet creation.
// Returns (result, done, error):
//   - done=true: restore is complete (or not needed), continue to create StatefulSet
//   - done=false: restore is in progress, requeue with result
func (r *OpenClawInstanceReconciler) reconcileRestore(ctx context.Context, instance *openclawv1alpha1.OpenClawInstance) (result ctrl.Result, done bool, _ error) {
	logger := log.FromContext(ctx)

	// Skip if no restore requested
	if instance.Spec.RestoreFrom == "" {
		return ctrl.Result{}, true, nil
	}

	// Skip if already restored (idempotent)
	if instance.Status.RestoredFrom != "" {
		return ctrl.Result{}, true, nil
	}

	logger.Info("Restore from backup requested", "restoreFrom", instance.Spec.RestoreFrom)

	// Update phase to Restoring
	if instance.Status.Phase != openclawv1alpha1.PhaseRestoring {
		instance.Status.Phase = openclawv1alpha1.PhaseRestoring
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	// Get S3 credentials
	creds, err := r.getS3Credentials(ctx)
	if err != nil {
		logger.Error(err, "Failed to get S3 credentials for restore")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "RestoreCredentialsFailed", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, false, nil
	}

	// Reconcile mirror Secret for secretKeyRef (no-op for env-auth mode)
	if mirrorErr := r.reconcileS3MirrorSecret(ctx, instance, creds); mirrorErr != nil {
		return ctrl.Result{}, false, mirrorErr
	}

	jobName := restoreJobName(instance)
	pvcName := pvcNameForInstance(instance)

	// Check for existing restore Job
	existingJob, err := r.getJob(ctx, jobName, instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}

	if apierrors.IsNotFound(err) || existingJob == nil {
		// Create restore Job
		labels := backupLabels(instance, "restore")
		job := buildRcloneJob(jobName, instance.Namespace, pvcName, instance.Spec.RestoreFrom, labels, creds, false, instance.Spec.Availability.NodeSelector, instance.Spec.Availability.Tolerations, instance.Spec.Backup.ServiceAccountName, mirrorSecretName(instance), &instance.Spec.Backup)

		// Set owner reference
		if err := controllerutil.SetControllerReference(instance, job, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}

		instance.Status.RestoreJobName = jobName
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, false, err
		}

		logger.Info("Creating restore Job", "job", jobName, "restoreFrom", instance.Spec.RestoreFrom)
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
			}
			return ctrl.Result{}, false, err
		}
		r.Recorder.Event(instance, corev1.EventTypeNormal, "RestoreStarted",
			fmt.Sprintf("Restore Job %s created, source: %s", jobName, instance.Spec.RestoreFrom))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
	}

	// Job exists — check status
	finished, condType := isJobFinished(existingJob)
	if !finished {
		logger.Info("Restore Job still running", "job", jobName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, false, nil
	}

	if condType == batchv1.JobFailed {
		logger.Error(nil, "Restore Job failed", "job", jobName)
		r.Recorder.Event(instance, corev1.EventTypeWarning, "RestoreFailed",
			fmt.Sprintf("Restore Job %s failed. Delete the Job to retry.", jobName))

		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:    openclawv1alpha1.ConditionTypeRestoreComplete,
			Status:  metav1.ConditionFalse,
			Reason:  "RestoreFailed",
			Message: fmt.Sprintf("Restore Job %s failed", jobName),
		})
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, false, nil
	}

	// Restore succeeded
	logger.Info("Restore Job completed successfully", "job", jobName, "restoreFrom", instance.Spec.RestoreFrom)
	r.Recorder.Event(instance, corev1.EventTypeNormal, "RestoreComplete",
		fmt.Sprintf("Restore completed from %s", instance.Spec.RestoreFrom))

	// Set status
	instance.Status.RestoredFrom = instance.Spec.RestoreFrom
	instance.Status.Phase = openclawv1alpha1.PhaseProvisioning
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:    openclawv1alpha1.ConditionTypeRestoreComplete,
		Status:  metav1.ConditionTrue,
		Reason:  "RestoreSucceeded",
		Message: fmt.Sprintf("Restored from %s", instance.Spec.RestoreFrom),
	})
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, false, err
	}

	// Clear spec.RestoreFrom (patch the spec to remove it)
	original := instance.DeepCopy()
	instance.Spec.RestoreFrom = ""
	if err := r.Patch(ctx, instance, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("failed to clear spec.restoreFrom: %w", err)
	}

	return ctrl.Result{}, true, nil
}
