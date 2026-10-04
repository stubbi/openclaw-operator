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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// This suite verifies the fix for #607: on fsGroup-only PVCs the data volume
// root stays owned by root, and OpenClaw >= 2026.9 fails with
// "EPERM: operation not permitted, fchmod" when it tightens directory modes
// on ~/.openclaw. The operator now runs init-data-owner first, as root with
// only CAP_CHOWN, to hand the volume root to the pod UID.
var _ = Describe("Data volume ownership init container (#607)", func() {
	Context("When creating a persistent OpenClawInstance", func() {
		var namespace string

		BeforeEach(func() {
			namespace = "test-init-data-owner-" + time.Now().Format("20060102150405")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
			Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
		})

		AfterEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("Should run init-data-owner first with root + CAP_CHOWN only", func() {
			if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
				Skip("Skipping resource validation in minimal mode")
			}

			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "init-data-owner",
					Namespace: namespace,
					Annotations: map[string]string{
						"openclaw.rocks/skip-backup": "true",
					},
				},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{
					Image: openclawv1alpha1.ImageSpec{
						Repository: "ghcr.io/openclaw/openclaw",
						Tag:        "latest",
					},
					Storage: openclawv1alpha1.StorageSpec{
						Persistence: openclawv1alpha1.PersistenceSpec{
							Size: "1Gi",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, instance)).Should(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      resources.StatefulSetName(instance),
					Namespace: namespace,
				}, sts)
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			initContainers := sts.Spec.Template.Spec.InitContainers
			Expect(initContainers).NotTo(BeEmpty())
			c := initContainers[0]
			Expect(c.Name).To(Equal(resources.DataOwnerInitContainerName),
				"ownership fix must be the first init container so nothing writes to the volume before it")

			sc := c.SecurityContext
			Expect(sc).NotTo(BeNil())
			Expect(sc.RunAsUser).To(HaveValue(BeEquivalentTo(0)))
			Expect(sc.RunAsNonRoot).To(HaveValue(BeFalse()))
			Expect(sc.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
			Expect(sc.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
			Expect(sc.Capabilities).NotTo(BeNil())
			Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(sc.Capabilities.Add).To(ConsistOf(corev1.Capability("CHOWN")))

			var dataMount *corev1.VolumeMount
			for i := range c.VolumeMounts {
				if c.VolumeMounts[i].Name == "data" {
					dataMount = &c.VolumeMounts[i]
				}
			}
			Expect(dataMount).NotTo(BeNil(), "init-data-owner must mount the data volume")
			Expect(dataMount.SubPath).To(BeEmpty(), "init-data-owner must see the volume root, not a SubPath")
			Expect(dataMount.MountPath).To(Equal("/data"))
		})

		It("Should omit init-data-owner when storage.fixOwnership is false", func() {
			if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
				Skip("Skipping resource validation in minimal mode")
			}

			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "init-data-owner-off",
					Namespace: namespace,
					Annotations: map[string]string{
						"openclaw.rocks/skip-backup": "true",
					},
				},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{
					Image: openclawv1alpha1.ImageSpec{
						Repository: "ghcr.io/openclaw/openclaw",
						Tag:        "latest",
					},
					Storage: openclawv1alpha1.StorageSpec{
						FixOwnership: resources.Ptr(false),
						Persistence: openclawv1alpha1.PersistenceSpec{
							Size: "1Gi",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, instance)).Should(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      resources.StatefulSetName(instance),
					Namespace: namespace,
				}, sts)
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			for _, c := range sts.Spec.Template.Spec.InitContainers {
				Expect(c.Name).NotTo(Equal(resources.DataOwnerInitContainerName))
			}
		})
	})
})
