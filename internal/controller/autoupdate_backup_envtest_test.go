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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

var _ = Describe("legacy pre-update Job deadline API", func() {
	It("retrofits deadlines and fences concurrent changes with the real API server", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "backup-deadline-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		// Exercise the shipped permissions rather than envtest's admin identity.
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		var shippedRole rbacv1.ClusterRole
		Expect(yaml.Unmarshal(data, &shippedRole)).To(Succeed())
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: namespace.Name},
			Rules:      shippedRole.Rules,
		}
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, role)).To(Succeed()) })
		userConfig := rest.CopyConfig(cfg)
		userConfig.Impersonate = rest.ImpersonationConfig{UserName: namespace.Name, Groups: []string{"system:authenticated"}}
		operatorClient, err := client.New(userConfig, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsForbidden(operatorClient.List(ctx, &batchv1.JobList{}, client.InNamespace(namespace.Name)))).To(BeTrue())
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: namespace.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: "User", Name: namespace.Name, APIGroup: rbacv1.GroupName}},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, binding)).To(Succeed()) })
		Eventually(func() error {
			return operatorClient.List(ctx, &batchv1.JobList{}, client.InNamespace(namespace.Name))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		r := &OpenClawInstanceReconciler{Client: operatorClient}
		for _, oldDeadline := range []*int64{nil, resources.Ptr(int64(3600))} {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "legacy-", Namespace: namespace.Name},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: oldDeadline,
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "fixture", Image: "busybox:1.37", Command: []string{"sleep", "300"}}},
					}},
				},
			}
			// envtest has no Job controller: no container is started. This tests
			// API mutation/preconditions, not the kube-controller-manager timer.
			Expect(k8sClient.Create(ctx, job)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, job)).To(Succeed()) })
			started := metav1.Now()
			job.Status.StartTime = &started
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
			end := job.Status.StartTime.Add(5 * time.Minute)
			updated, err := r.boundPreUpdateBackupJob(ctx, job, end)
			Expect(err).NotTo(HaveOccurred())
			Expect(updated).To(BeTrue())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(300)))
			version := job.ResourceVersion
			updated, err = r.boundPreUpdateBackupJob(ctx, job, end.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(updated).To(BeFalse(), "do not extend an existing shorter execution bound")
			Expect(job.ResourceVersion).To(Equal(version))

			stale := job.DeepCopy()
			original := job.DeepCopy()
			job.Labels = map[string]string{"concurrent-writer": "changed"}
			Expect(k8sClient.Patch(ctx, job, client.MergeFrom(original))).To(Succeed())
			updated, err = r.boundPreUpdateBackupJob(ctx, stale, end.Add(-time.Minute))
			Expect(updated).To(BeFalse())
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "reconcile must re-read a concurrent Job change")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(300)))
		}
	})
})
