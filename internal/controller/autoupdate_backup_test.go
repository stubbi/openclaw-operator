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
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// Regression for #631: an unfinished pre-update Job must not keep the instance
// stopped indefinitely, including Jobs created before deadline support existed.
func TestPreUpdateBackupExpiredJobRecoversInstance(t *testing.T) {
	r, instance, sts, _ := preUpdateFixture(t)
	testCtx, c := context.Background(), r.Client
	for range 8 {
		if err := c.Get(testCtx, client.ObjectKeyFromObject(instance), instance); err != nil {
			t.Fatal(err)
		}
		if instance.Status.AutoUpdate.PendingVersion == "" {
			break
		}
		if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(testCtx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AutoUpdate.PendingVersion != "" || *sts.Spec.Replicas != 1 {
		t.Fatalf("stalled backup left instance offline: pending=%q replicas=%d", instance.Status.AutoUpdate.PendingVersion, *sts.Spec.Replicas)
	}
	if instance.Spec.Image.Tag != "2026.9.9" || instance.Status.AutoUpdate.LastUpdateError == "" {
		t.Fatalf("aborted update must keep the old image and report the failure: %+v", instance.Status.AutoUpdate)
	}
}

func preUpdateFixture(t *testing.T) (*OpenClawInstanceReconciler, *openclawv1alpha1.OpenClawInstance, *appsv1.StatefulSet, *batchv1.Job) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme, openclawv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	instance := &openclawv1alpha1.OpenClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "deadline-test", Namespace: "default", UID: "test-uid"},
		Spec:       openclawv1alpha1.OpenClawInstanceSpec{Image: openclawv1alpha1.ImageSpec{Tag: "2026.9.9"}},
		Status: openclawv1alpha1.OpenClawInstanceStatus{
			Phase:      openclawv1alpha1.PhaseUpdating,
			AutoUpdate: openclawv1alpha1.AutoUpdateStatus{PendingVersion: "2026.10.1", UpdatePhase: updatePhaseBackingUp},
		},
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: resources.StatefulSetName(instance), Namespace: instance.Namespace}, Spec: appsv1.StatefulSetSpec{Replicas: resources.Ptr(int32(0))}}
	old := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: preUpdateBackupJobName(instance), Namespace: instance.Namespace, CreationTimestamp: old}, Status: batchv1.JobStatus{StartTime: &old}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: BackupSecretName, Namespace: "default"}, Data: map[string][]byte{"S3_BUCKET": []byte("test"), "S3_ENDPOINT": []byte("http://127.0.0.1:1")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance, &batchv1.Job{}).WithObjects(instance, sts, job, secret).Build()
	r := &OpenClawInstanceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100), OperatorNamespace: "default"}
	return r, instance, sts, job
}

func reloadPreUpdate(t *testing.T, r *OpenClawInstanceReconciler, instance *openclawv1alpha1.OpenClawInstance) {
	t.Helper()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
}

func TestPreUpdateBackupDeadlineIncludesPodTermination(t *testing.T) {
	r, instance, sts, job := preUpdateFixture(t)
	testCtx := context.Background()
	if err := r.Delete(testCtx, job); err != nil {
		t.Fatal(err)
	}
	old := metav1.NewTime(time.Now().Add(-6 * time.Minute))
	instance.Spec.AutoUpdate.BackupTimeout = "5m"
	if err := r.Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.AutoUpdate.BackupStartedAt = &old
	if err := r.Status().Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-still-terminating", Namespace: instance.Namespace, Labels: resources.SelectorLabels(instance)}}
	if err := r.Create(testCtx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	reloadPreUpdate(t, r, instance)
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AutoUpdate.PendingVersion != "" || *sts.Spec.Replicas != 1 || !strings.Contains(instance.Status.AutoUpdate.LastUpdateError, "timed out") {
		t.Fatalf("pod termination was not bounded: %+v, replicas=%d", instance.Status.AutoUpdate, *sts.Spec.Replicas)
	}
}

func TestPreUpdateBackupPersistsBudgetAndConfiguresJob(t *testing.T) {
	r, instance, sts, job := preUpdateFixture(t)
	testCtx := context.Background()
	if err := r.Delete(testCtx, job); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Replicas = resources.Ptr(int32(1))
	if err := r.Update(testCtx, sts); err != nil {
		t.Fatal(err)
	}
	instance.Spec.AutoUpdate.BackupTimeout = "5m"
	instance.Spec.Backup = openclawv1alpha1.BackupSpec{Excludes: []string{"/npm/**"}, Transfers: resources.Ptr(int32(32)), Checkers: resources.Ptr(int32(64)), FastList: true}
	if err := r.Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.AutoUpdate.UpdatePhase = ""
	if err := r.Status().Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	reloadPreUpdate(t, r, instance)
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AutoUpdate.BackupStartedAt == nil || *sts.Spec.Replicas != 1 {
		t.Fatal("must persist deadline before stopping the instance")
	}
	started := metav1.NewTime(time.Now().Add(-2 * time.Minute).Truncate(time.Second))
	instance.Status.AutoUpdate.BackupStartedAt = &started
	if err := r.Status().Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	// A fresh reconciler must retain the budget rather than starting it over.
	restarted := &OpenClawInstanceReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder, OperatorNamespace: r.OperatorNamespace}
	for range 2 {
		reloadPreUpdate(t, restarted, instance)
		if _, err := restarted.driveUpdateStateMachine(testCtx, instance); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Get(testCtx, client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatal(err)
	}
	deadline := job.Spec.ActiveDeadlineSeconds
	if deadline == nil || *deadline > 180 || *deadline < 175 {
		t.Fatalf("deadline does not include preparation time: %v", deadline)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"--exclude=/npm/**", "--transfers=32", "--checkers=64", "--fast-list"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s in %s", want, args)
		}
	}
	reloadPreUpdate(t, r, instance)
	if !instance.Status.AutoUpdate.BackupStartedAt.Equal(&started) {
		t.Fatal("backup budget reset across reconciles")
	}
}

type failingBackupClient struct {
	client.Client
	failDelete, failScale bool
}

func (c *failingBackupClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*batchv1.Job); ok && c.failDelete {
		c.failDelete = false
		return errors.New("injected delete failure")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *failingBackupClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*appsv1.StatefulSet); ok && c.failScale {
		c.failScale = false
		return errors.New("injected scale failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestPreUpdateBackupAbortWaitsForPodsAndRetries(t *testing.T) {
	r, instance, sts, _ := preUpdateFixture(t)
	testCtx := context.Background()
	c := &failingBackupClient{Client: r.Client, failDelete: true, failScale: true}
	r.Client = c
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "backup-worker", Namespace: instance.Namespace, Labels: backupLabels(instance, "pre-update-backup")}}
	if err := r.Create(testCtx, pod); err != nil {
		t.Fatal(err)
	}
	// Adopt the pre-existing Job and persist its age.
	if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	reloadPreUpdate(t, r, instance)
	if result, err := r.driveUpdateStateMachine(testCtx, instance); err == nil || result.RequeueAfter == 0 {
		t.Fatal("expected retry on delete error")
	}
	for range 2 {
		reloadPreUpdate(t, r, instance)
		if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
			t.Fatal(err)
		}
	}
	reloadPreUpdate(t, r, instance)
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AutoUpdate.UpdatePhase != updatePhaseAbortingBackup || *sts.Spec.Replicas != 0 {
		t.Fatal("gateway restarted while backup Pod still exists")
	}
	if err := r.Delete(testCtx, pod); err != nil {
		t.Fatal(err)
	}
	if result, err := r.driveUpdateStateMachine(testCtx, instance); err == nil || result.RequeueAfter == 0 {
		t.Fatal("expected retry on scale error")
	}
	reloadPreUpdate(t, r, instance)
	if instance.Status.AutoUpdate.PendingVersion == "" {
		t.Fatal("lost retry state before scale-up succeeded")
	}
	if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	reloadPreUpdate(t, r, instance)
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AutoUpdate.PendingVersion != "" || *sts.Spec.Replicas != 1 || shouldCheckForUpdate(instance) {
		t.Fatal("recovery must restore service and delay next retry")
	}
}

func TestPreUpdateBackupSuccessRecordsRollbackDestination(t *testing.T) {
	r, instance, sts, job := preUpdateFixture(t)
	testCtx := context.Background()
	path := "backups/default/deadline-test/pre-update-snapshot"
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "rclone", Args: []string{"sync", "/data/", ":s3:test/" + path}}}
	if err := r.Update(testCtx, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(testCtx, job); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reloadPreUpdate(t, r, instance)
		if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
			t.Fatal(err)
		}
	}
	reloadPreUpdate(t, r, instance)
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if instance.Spec.Image.Tag != "2026.10.1" || instance.Status.AutoUpdate.PreUpdateBackupPath != path || *sts.Spec.Replicas != 1 {
		t.Fatalf("completed backup did not advance safely: %+v", instance.Status.AutoUpdate)
	}
}

func TestPreUpdateBackupDoesNotReusePreviousAttempt(t *testing.T) {
	r, instance, _, job := preUpdateFixture(t)
	testCtx := context.Background()
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(testCtx, job); err != nil {
		t.Fatal(err)
	}
	instance.Status.AutoUpdate.UpdatePhase = ""
	if err := r.Status().Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reloadPreUpdate(t, r, instance)
		if _, err := r.driveUpdateStateMachine(testCtx, instance); err != nil {
			t.Fatal(err)
		}
	}
	reloadPreUpdate(t, r, instance)
	if instance.Spec.Image.Tag != "2026.9.9" {
		t.Fatal("a previous snapshot must not authorize the next update")
	}
	if err := r.Get(testCtx, client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatal(err)
	}
	if done, _ := isJobFinished(job); done || job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("expected a fresh bounded backup Job")
	}
}
