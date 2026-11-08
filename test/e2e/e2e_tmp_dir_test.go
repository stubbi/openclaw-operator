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

// OpenClaw >= 2026.9.6 refuses to create a temporary workspace below a
// directory that is group/world writable without the sticky bit. Under
// fsGroup the root of the /tmp emptyDir is exactly that (mode 2777), so the
// startup doctor exits as soon as it has to install a configured plugin and
// the pod crash-loops. The operator therefore has init-tmp-dir create a 1777
// subdirectory and mounts that at /tmp in the main container.
//
// This spec pins the rendered shape. The behavior itself is exercised by the
// Chromium Full Integration suite, which boots OpenClaw with a provider key
// and waits for the main container to become ready.
var _ = Describe("Sticky /tmp for the main container", func() {
	var namespace string

	BeforeEach(func() {
		if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
			Skip("Skipping resource validation in minimal mode")
		}
		namespace = "test-tmp-dir-" + time.Now().Format("20060102150405")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	It("Should create the sticky subdirectory and mount it at /tmp", func() {
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tmp-dir",
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

		var initTmp *corev1.Container
		for i := range sts.Spec.Template.Spec.InitContainers {
			if sts.Spec.Template.Spec.InitContainers[i].Name == resources.TmpDirInitContainerName {
				initTmp = &sts.Spec.Template.Spec.InitContainers[i]
			}
		}
		Expect(initTmp).NotTo(BeNil(), "init-tmp-dir must always be rendered")
		Expect(initTmp.SecurityContext).NotTo(BeNil())
		Expect(initTmp.SecurityContext.RunAsUser).To(BeNil(),
			"init-tmp-dir must run as the pod UID so that UID owns the directory")
		Expect(initTmp.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
		Expect(initTmp.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "tmp", MountPath: "/tmp-volume"}))

		var mainTmp *corev1.VolumeMount
		for i := range sts.Spec.Template.Spec.Containers {
			c := &sts.Spec.Template.Spec.Containers[i]
			if c.Name != "openclaw" {
				continue
			}
			for j := range c.VolumeMounts {
				if c.VolumeMounts[j].MountPath == "/tmp" {
					mainTmp = &c.VolumeMounts[j]
				}
			}
		}
		Expect(mainTmp).NotTo(BeNil(), "main container must mount /tmp")
		Expect(mainTmp.Name).To(Equal("tmp"))
		Expect(mainTmp.SubPath).To(Equal(resources.MainTmpSubPath),
			"mounting the emptyDir root would expose its 2777 mode to OpenClaw")
	})
})
