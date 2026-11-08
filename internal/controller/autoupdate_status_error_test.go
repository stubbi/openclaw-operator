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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// A failed progress-status write must not fall through to normal resource
// reconciliation and restart the gateway while its backup still reads the PVC.
func TestPreUpdateBackupStatusErrorDoesNotResumeGateway(t *testing.T) {
	testCtx := context.Background()
	r, instance, sts, job := preUpdateFixture(t)
	if err := kubescheme.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	instance.Finalizers = []string{FinalizerName}
	instance.Spec.AutoUpdate.BackupTimeout = "5m"
	if err := r.Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	started := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	instance.Status.AutoUpdate.BackupStartedAt = &started
	if err := r.Status().Update(testCtx, instance); err != nil {
		t.Fatal(err)
	}
	job.CreationTimestamp = started
	job.Spec.ActiveDeadlineSeconds = resources.Ptr(int64(300))
	if err := r.Update(testCtx, job); err != nil {
		t.Fatal(err)
	}
	job.Status.StartTime = &started
	job.Status.Active = 1
	if err := r.Status().Update(testCtx, job); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	desired := resources.BuildStatefulSet(instance, "", nil, nil, nil)
	resources.NormalizeStatefulSet(desired)
	desired.ResourceVersion = sts.ResourceVersion
	desired.Spec.Replicas = resources.Ptr(int32(0))
	if err := r.Update(testCtx, desired); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected backup progress status update failure")
	faults := &backupProgressStatusErrorClient{Client: r.Client, err: injected}
	r.Client = faults
	result, reconcileErr := r.Reconcile(testCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if faults.failures != 1 {
		t.Fatalf("progress status failure was not exercised exactly once: %d; reconcile=%v", faults.failures, reconcileErr)
	}
	if err := r.Get(testCtx, client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatalf("running backup must still exist for this scenario: %v", err)
	}
	if job.Status.Active != 1 {
		t.Fatalf("backup is no longer active, so this scenario does not qualify: %+v", job.Status)
	}
	if err := r.Get(testCtx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Replicas == nil {
		t.Fatal("gateway replica count became implicit while its backup remained active")
	}
	if *sts.Spec.Replicas != 0 {
		t.Fatalf("gateway resumed while its backup was active after a status error: replicas=%d; result=%+v; reconcile=%v", *sts.Spec.Replicas, result, reconcileErr)
	}
	if reconcileErr != nil && !errors.Is(reconcileErr, injected) {
		t.Fatalf("unrelated reconciliation error does not qualify this regression: %v", reconcileErr)
	}
	if reconcileErr == nil && !result.Requeue && result.RequeueAfter <= 0 {
		t.Fatal("status error must request another reconcile")
	}
}

type backupProgressStatusErrorClient struct {
	client.Client
	err      error
	failures int
}

func (c *backupProgressStatusErrorClient) Status() client.SubResourceWriter {
	return &backupProgressStatusErrorWriter{SubResourceWriter: c.Client.Status(), faults: c}
}

type backupProgressStatusErrorWriter struct {
	client.SubResourceWriter
	faults *backupProgressStatusErrorClient
}

func (w *backupProgressStatusErrorWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	inst, ok := obj.(*openclawv1alpha1.OpenClawInstance)
	if ok && inst.Status.AutoUpdate.PendingVersion != "" && inst.Status.AutoUpdate.UpdatePhase == updatePhaseBackingUp && w.faults.failures == 0 {
		w.faults.failures++
		return w.faults.err
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

var _ client.Client = (*backupProgressStatusErrorClient)(nil)
