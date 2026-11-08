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
	"fmt"
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
// This spec checks the rendered shape and runs the generated helper in a
// probe pod with the same security contexts and /tmp volume. This exercises
// kubelet startup and filesystem permissions without a provider key.
var _ = Describe("Sticky /tmp for the main container", func() {
	var namespace string

	BeforeEach(func() {
		if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
			Skip("Skipping resource validation in minimal mode")
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-tmp-dir-"}}
		Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
		namespace = ns.Name
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	DescribeTable("Should create the sticky subdirectory and mount it at /tmp", func(uid *int64, nonRoot *bool, wantNonRoot bool) {
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
				Security: openclawv1alpha1.SecuritySpec{
					ContainerSecurityContext: &openclawv1alpha1.ContainerSecurityContextSpec{
						RunAsUser: uid, RunAsNonRoot: nonRoot,
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

		var initTmp *corev1.Container
		for i := range sts.Spec.Template.Spec.InitContainers {
			if sts.Spec.Template.Spec.InitContainers[i].Name == resources.TmpDirInitContainerName {
				initTmp = &sts.Spec.Template.Spec.InitContainers[i]
			}
		}
		Expect(initTmp).NotTo(BeNil(), "init-tmp-dir must always be rendered")
		Expect(initTmp.SecurityContext).NotTo(BeNil())
		Expect(initTmp.SecurityContext.RunAsUser).To(Equal(uid),
			"init-tmp-dir must follow the gateway UID override or inherit the pod UID")
		Expect(initTmp.SecurityContext.RunAsNonRoot).To(HaveValue(Equal(wantNonRoot)))
		Expect(initTmp.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "tmp", MountPath: "/tmp-volume"}))

		var main *corev1.Container
		var mainTmp *corev1.VolumeMount
		for i := range sts.Spec.Template.Spec.Containers {
			c := &sts.Spec.Template.Spec.Containers[i]
			if c.Name != "openclaw" {
				continue
			}
			main = c
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

		By("running the generated helper and checking the mounted directory")
		Expect(main).NotTo(BeNil())
		Expect(main.SecurityContext).NotTo(BeNil())
		Expect(sts.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
		wantUID := sts.Spec.Template.Spec.SecurityContext.RunAsUser
		if main.SecurityContext.RunAsUser != nil {
			wantUID = main.SecurityContext.RunAsUser
		}
		Expect(wantUID).NotTo(BeNil())

		var tmpVolume *corev1.Volume
		for i := range sts.Spec.Template.Spec.Volumes {
			if sts.Spec.Template.Spec.Volumes[i].Name == mainTmp.Name {
				tmpVolume = &sts.Spec.Template.Spec.Volumes[i]
			}
		}
		Expect(tmpVolume).NotTo(BeNil())
		Expect(tmpVolume.EmptyDir).NotTo(BeNil())

		probe := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "tmp-dir-probe", Namespace: namespace},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				ActiveDeadlineSeconds:        resources.Ptr(int64(120)),
				AutomountServiceAccountToken: resources.Ptr(false),
				SecurityContext:              sts.Spec.Template.Spec.SecurityContext.DeepCopy(),
				InitContainers:               []corev1.Container{*initTmp.DeepCopy()},
				Volumes:                      []corev1.Volume{*tmpVolume.DeepCopy()},
				Containers: []corev1.Container{{
					Name:            "check-tmp",
					Image:           initTmp.Image,
					ImagePullPolicy: initTmp.ImagePullPolicy,
					SecurityContext: main.SecurityContext.DeepCopy(),
					VolumeMounts:    []corev1.VolumeMount{*mainTmp},
					Command: []string{"sh", "-c", fmt.Sprintf(`set -eu
test "$(id -u)" = "%[1]d"
test "$(stat -c '%%u %%a' /tmp)" = "%[1]d 1777"
touch /tmp/write-check
rm /tmp/write-check`, *wantUID)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, probe)).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: probe.Name, Namespace: namespace}, probe)).To(Succeed())
			g.Expect(probe.Status.Phase).To(Equal(corev1.PodSucceeded),
				"helper status: %+v; probe status: %+v", probe.Status.InitContainerStatuses, probe.Status.ContainerStatuses)
		}, 180*time.Second, 2*time.Second).Should(Succeed())
		Expect(probe.Status.InitContainerStatuses).To(HaveLen(1))
		Expect(probe.Status.InitContainerStatuses[0].Name).To(Equal(resources.TmpDirInitContainerName))
		Expect(probe.Status.InitContainerStatuses[0].State.Terminated).NotTo(BeNil())
		Expect(probe.Status.InitContainerStatuses[0].State.Terminated.ExitCode).To(BeZero())
	},
		Entry("with default non-root settings", nil, nil, true),
		Entry("with a non-root container UID override", resources.Ptr(int64(2000)), nil, true),
		Entry("with an explicit root container override", resources.Ptr(int64(0)), resources.Ptr(false), false),
	)
})
