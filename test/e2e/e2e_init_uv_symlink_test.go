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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// Regression test for #626: init-uv crash-looped with "cp: not writing
// through dangling symlink" when ~/.local/bin/uv was an absolute symlink that
// a gateway-side script had created. The test runs the operator-rendered
// init-uv container, with its real image, in a standalone pod whose data
// volume already holds such a dangling link, and checks that it completes and
// leaves a regular uv binary behind.
var _ = Describe("init-uv with a dangling uv symlink (#626)", func() {
	var namespace string

	BeforeEach(func() {
		namespace = "test-init-uv-symlink-" + time.Now().Format("20060102150405")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	DescribeTable("handles gateway-created links", func(seed, verify string, mounts []corev1.VolumeMount) {
		if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
			Skip("Skipping resource validation in minimal mode")
		}

		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "uv-symlink", Namespace: namespace},
		}
		sts := resources.BuildStatefulSet(instance, "", nil, nil, nil)
		var initUv *corev1.Container
		for i := range sts.Spec.Template.Spec.InitContainers {
			if sts.Spec.Template.Spec.InitContainers[i].Name == "init-uv" {
				initUv = sts.Spec.Template.Spec.InitContainers[i].DeepCopy()
			}
		}
		Expect(initUv).NotTo(BeNil(), "init-uv missing from the rendered StatefulSet")

		restricted := func() *corev1.SecurityContext {
			return &corev1.SecurityContext{
				AllowPrivilegeEscalation: resources.Ptr(false),
				ReadOnlyRootFilesystem:   resources.Ptr(true),
				RunAsNonRoot:             resources.Ptr(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "init-uv-symlink", Namespace: namespace},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser:  resources.Ptr(int64(1000)),
					RunAsGroup: resources.Ptr(int64(1000)),
					FSGroup:    resources.Ptr(int64(1000)),
				},
				InitContainers: []corev1.Container{
					{
						// What the reporter's agent-side bootstrap left behind:
						// a link to a gateway path whose target is gone.
						Name:            "seed-dangling-link",
						Image:           "docker.io/library/busybox:1.37",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{"sh", "-c",
							seed},
						SecurityContext: restricted(),
						VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
					},
					*initUv,
				},
				Containers: []corev1.Container{
					{
						Name:            "verify",
						Image:           "docker.io/library/busybox:1.37",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{"sh", "-c",
							verify},
						SecurityContext: restricted(),
						VolumeMounts:    mounts,
					},
				},
				Volumes: []corev1.Volume{
					{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).Should(Succeed())

		Eventually(func(g Gomega) {
			got := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, got)).To(Succeed())
			for _, cs := range got.Status.InitContainerStatuses {
				if cs.Name == "init-uv" && cs.State.Terminated != nil {
					g.Expect(cs.State.Terminated.ExitCode).To(BeZero(),
						"init-uv failed: %s", cs.State.Terminated.Message)
				}
			}
			g.Expect(got.Status.Phase).To(Equal(corev1.PodSucceeded),
				"pod phase %s, init: %v, containers: %v",
				got.Status.Phase, got.Status.InitContainerStatuses, got.Status.ContainerStatuses)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	},
		Entry("replaces a dangling link",
			"mkdir -p /data/.local/bin && ln -sfn /home/openclaw/.openclaw/tools/uv/gone/uv /data/.local/bin/uv",
			`f=/data/.local/bin/uv; [ ! -L "$f" ] && [ -f "$f" ] && [ -x "$f" ]`,
			[]corev1.VolumeMount{{Name: "data", MountPath: "/data"}}),
		Entry("preserves a relative link crossing the gateway mounts",
			"mkdir -p /data/.local/bin /data/tools && echo pinned > /data/tools/uv && chmod +x /data/tools/uv && ln -s ../../.openclaw/tools/uv /data/.local/bin/uv",
			`f=/home/openclaw/.local/bin/uv; [ -L "$f" ] && [ "$(readlink "$f")" = ../../.openclaw/tools/uv ] && [ "$(cat "$f")" = pinned ]`,
			[]corev1.VolumeMount{{Name: "data", MountPath: "/home/openclaw/.openclaw"}, {Name: "data", MountPath: "/home/openclaw/.local", SubPath: ".local"}}),
	)
})
