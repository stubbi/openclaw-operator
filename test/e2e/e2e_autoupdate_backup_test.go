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

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/controller"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// Run on a disposable cluster with CRDs installed and no operator deployment:
// OPENCLAW_BACKUP_E2E=1 KUBECONFIG=... go test ./test/e2e -run '^TestPreUpdateBackupRecoveryOnCluster$' -v
// Reconciliation is driven explicitly so elapsed-time injection is deterministic;
// StatefulSets, PVCs, Jobs, deadlines, Pods, and garbage collection are real.
func TestPreUpdateBackupRecoveryOnCluster(t *testing.T) {
	if os.Getenv("OPENCLAW_BACKUP_E2E") != "1" {
		t.Skip("requires OPENCLAW_BACKUP_E2E=1 and a disposable cluster without an operator")
	}
	testCtx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, openclawv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	clusterConfig, err := config.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(clusterConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"controller-timeout", "kubernetes-deadline"} {
		t.Run(mode, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("backup-%s-%d", mode, time.Now().UnixNano())}}
			if err := c.Create(testCtx, ns); err != nil {
				t.Fatal(err)
			}
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "backup-recovery", Namespace: ns.Name, Finalizers: []string{controller.FinalizerName}},
				Spec:       openclawv1alpha1.OpenClawInstanceSpec{Image: openclawv1alpha1.ImageSpec{Tag: "2026.9.9"}, AutoUpdate: openclawv1alpha1.AutoUpdateSpec{BackupTimeout: "5m"}, Backup: openclawv1alpha1.BackupSpec{Excludes: []string{"/cache/**"}, Transfers: resources.Ptr(int32(32)), FastList: true}},
			}
			t.Cleanup(func() {
				current := &openclawv1alpha1.OpenClawInstance{}
				if getErr := c.Get(testCtx, client.ObjectKeyFromObject(instance), current); getErr == nil {
					current.Finalizers = nil
					_ = c.Update(testCtx, current)
				}
				_ = c.Delete(testCtx, ns)
			})
			if err := c.Create(testCtx, instance); err != nil {
				t.Fatal(err)
			}
			instance.Status.Phase = openclawv1alpha1.PhaseUpdating
			instance.Status.AutoUpdate.PendingVersion = "2026.10.1"
			if err := c.Status().Update(testCtx, instance); err != nil {
				t.Fatal(err)
			}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: resources.PVCName(instance), Namespace: ns.Name}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("16Mi")}}}}
			if err := c.Create(testCtx, pvc); err != nil {
				t.Fatal(err)
			}
			labels := resources.SelectorLabels(instance)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: resources.StatefulSetName(instance), Namespace: ns.Name}, Spec: appsv1.StatefulSetSpec{
				Replicas: resources.Ptr(int32(1)), ServiceName: "unused", Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: resources.Ptr(int64(1)), SecurityContext: &corev1.PodSecurityContext{RunAsUser: resources.Ptr(int64(1000)), FSGroup: resources.Ptr(int64(1000))},
					Containers: []corev1.Container{{Name: "gateway", Image: "busybox:1.37", ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "-c", "sleep 3600"}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
					Volumes:    []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}},
				}},
			}}
			if err := c.Create(testCtx, sts); err != nil {
				t.Fatal(err)
			}
			waitBackupCluster(t, func() bool {
				return c.Get(testCtx, client.ObjectKeyFromObject(sts), sts) == nil && sts.Status.ReadyReplicas == 1
			})
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: controller.BackupSecretName, Namespace: ns.Name}, Data: map[string][]byte{"S3_BUCKET": []byte("test"), "S3_ENDPOINT": []byte("http://127.0.0.1:1"), "S3_ACCESS_KEY_ID": []byte("test"), "S3_SECRET_ACCESS_KEY": []byte("test")}}
			if err := c.Create(testCtx, secret); err != nil {
				t.Fatal(err)
			}
			r := &controller.OpenClawInstanceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100), OperatorNamespace: ns.Name}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: instance.Name + "-pre-update-backup", Namespace: ns.Name}}
			waitBackupCluster(t, func() bool {
				if _, reconcileErr := r.Reconcile(testCtx, request); reconcileErr != nil {
					t.Fatal(reconcileErr)
				}
				return c.Get(testCtx, client.ObjectKeyFromObject(job), job) == nil && job.Status.Active > 0
			})
			if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds > 300 || *job.Spec.ActiveDeadlineSeconds < 200 {
				t.Fatalf("missing remaining execution budget: %v", job.Spec.ActiveDeadlineSeconds)
			}
			args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
			for _, want := range []string{"--exclude=/cache/**", "--transfers=32", "--fast-list"} {
				if !strings.Contains(args, want) {
					t.Fatalf("missing %s", want)
				}
			}
			if err := c.Get(testCtx, client.ObjectKeyFromObject(instance), instance); err != nil {
				t.Fatal(err)
			}
			if mode == "controller-timeout" {
				started := metav1.NewTime(time.Now().Add(-6 * time.Minute))
				instance.Status.AutoUpdate.BackupStartedAt = &started
				if err := c.Status().Update(testCtx, instance); err != nil {
					t.Fatal(err)
				}
			} else {
				// Exercise Kubernetes termination independently of the operator.
				job.Spec.ActiveDeadlineSeconds = resources.Ptr(int64(1))
				if err := c.Update(testCtx, job); err != nil {
					t.Fatal(err)
				}
				waitBackupCluster(t, func() bool {
					if c.Get(testCtx, client.ObjectKeyFromObject(job), job) != nil {
						return false
					}
					for _, condition := range job.Status.Conditions {
						if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
							return condition.Reason == "DeadlineExceeded"
						}
					}
					return false
				})
			}
			waitBackupCluster(t, func() bool {
				if _, reconcileErr := r.Reconcile(testCtx, request); reconcileErr != nil {
					t.Fatal(reconcileErr)
				}
				if err := c.Get(testCtx, client.ObjectKeyFromObject(instance), instance); err != nil {
					t.Fatal(err)
				}
				return instance.Status.AutoUpdate.PendingVersion == ""
			})
			if instance.Spec.Image.Tag != "2026.9.9" || instance.Status.AutoUpdate.LastUpdateError == "" {
				t.Fatal("must abort without changing the image")
			}
			pods := &corev1.PodList{}
			if err := c.List(testCtx, pods, client.InNamespace(ns.Name), client.MatchingLabels{"openclaw.rocks/job-type": "pre-update-backup"}); err != nil {
				t.Fatal(err)
			}
			if len(pods.Items) != 0 {
				t.Fatal("backup Pod still exists after instance restart")
			}
			waitBackupCluster(t, func() bool {
				return c.Get(testCtx, client.ObjectKeyFromObject(sts), sts) == nil && *sts.Spec.Replicas == 1 && sts.Status.ReadyReplicas == 1
			})
			t.Logf("old version recovered with no backup Pods: %s", instance.Status.AutoUpdate.LastUpdateError)
		})
	}
}

func waitBackupCluster(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for backup cluster condition")
}
