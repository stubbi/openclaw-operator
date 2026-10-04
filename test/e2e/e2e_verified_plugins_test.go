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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

var _ = Describe("Verified plugin installation", func() {
	It("blocks unapproved artifacts and starts/restarts a gateway with a verified plugin", func() {
		if os.Getenv("E2E_VERIFIED_PLUGINS") != "true" {
			Skip("Set E2E_VERIFIED_PLUGINS=true in an isolated cluster to exercise real public npm installs")
		}
		namespace := fmt.Sprintf("verified-plugins-%d", time.Now().UnixNano())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, ns)).To(Succeed()) })

		pin := openclawv1alpha1.VerifiedPluginSpec{
			Package: "@openclaw/brave-plugin", Version: "2026.9.1",
			Integrity:          "sha512-4+j+eQTToV3k7Cb25MUL6h2uL8cJYyuLytfpd/sJK/HjR43dgKBqKpBsb1+I3w1Jr6PLpnjSf6/I3//3K0cdnA==",
			AcceptCapabilities: true,
		}
		createInstance := func(name string, plugin openclawv1alpha1.VerifiedPluginSpec) {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{"openclaw.rocks/skip-backup": "true"}},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{
					Config:          openclawv1alpha1.ConfigSpec{Raw: &openclawv1alpha1.RawConfig{RawExtension: runtime.RawExtension{Raw: []byte(`{"gateway":{"bind":"lan"}}`)}}},
					Image:           openclawv1alpha1.ImageSpec{Repository: "ghcr.io/openclaw/openclaw", Tag: "2026.9.4", Digest: "sha256:cc596b846506a5f4cfcee111394a2725f375f01cca2ebb492a161fd1b747f101"},
					VerifiedPlugins: []openclawv1alpha1.VerifiedPluginSpec{plugin},
					Gateway:         openclawv1alpha1.GatewaySpec{Enabled: resources.Ptr(false)},
					Observability:   openclawv1alpha1.ObservabilitySpec{Metrics: openclawv1alpha1.MetricsSpec{Enabled: resources.Ptr(false)}},
					Storage:         openclawv1alpha1.StorageSpec{Persistence: openclawv1alpha1.PersistenceSpec{Enabled: resources.Ptr(true), Size: "1Gi"}},
				},
			}
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
		}
		logs := func(name string) string {
			out, err := exec.CommandContext(ctx, "kubectl", "logs", "-n", namespace, name+"-0", "-c", "init-plugins").CombinedOutput()
			if err != nil {
				return ""
			}
			return string(out)
		}
		for _, tc := range []struct {
			name    string
			plugin  openclawv1alpha1.VerifiedPluginSpec
			message string
		}{
			{"bad-hash", openclawv1alpha1.VerifiedPluginSpec{Package: pin.Package, Version: pin.Version, Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)), AcceptCapabilities: true}, "registry dist.integrity does not match"},
			{"no-consent", openclawv1alpha1.VerifiedPluginSpec{Package: pin.Package, Version: pin.Version, Integrity: pin.Integrity}, "requires capability consent"},
		} {
			By("blocking gateway startup for " + tc.name)
			createInstance(tc.name, tc.plugin)
			Eventually(func() string { return logs(tc.name) }, 5*time.Minute, 2*time.Second).Should(ContainSubstring(tc.message))
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tc.name + "-0", Namespace: namespace}, pod)).To(Succeed())
			for _, status := range pod.Status.ContainerStatuses {
				Expect(status.State.Running).To(BeNil())
			}
		}

		assertGatewayPlugin := func() {
			// Ask the running gateway over RPC; a separate CLI disk inventory
			// alone would not establish the gateway's effective configuration.
			out, err := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace, "valid-0", "-c", "openclaw", "--", "openclaw", "gateway", "call", "plugins.inspect", "--params", `{"pluginId":"brave"}`, "--json").Output()
			Expect(err).NotTo(HaveOccurred(), "gateway plugin inspection: %s", out)
			var result struct {
				OK     bool `json:"ok"`
				Plugin struct {
					ID        string `json:"id"`
					Version   string `json:"version"`
					Installed bool   `json:"installed"`
					Enabled   bool   `json:"enabled"`
				} `json:"plugin"`
			}
			Expect(json.Unmarshal(out, &result)).To(Succeed(), "%s", out)
			Expect(result.OK).To(BeTrue(), "%s", out)
			Expect(result.Plugin.ID).To(Equal("brave"))
			Expect(result.Plugin.Version).To(Equal(pin.Version))
			Expect(result.Plugin.Installed).To(BeTrue())
			Expect(result.Plugin.Enabled).To(BeTrue())
		}

		By("starting a gateway with a verified plugin")
		createInstance("valid", pin)
		key := types.NamespacedName{Name: "valid-0", Namespace: namespace}
		ready := func() bool {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, key, pod); err != nil {
				return false
			}
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodReady {
					return c.Status == corev1.ConditionTrue
				}
			}
			return false
		}
		Eventually(ready, 5*time.Minute, 2*time.Second).Should(BeTrue())
		Expect(logs("valid")).To(ContainSubstring("installed verified " + pin.Integrity))
		assertGatewayPlugin()
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())
		oldUID := pod.UID
		By("reinstalling from the committed pin after a pod restart with the same PVC")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			current := &corev1.Pod{}
			return k8sClient.Get(ctx, key, current) == nil && current.UID != oldUID && ready()
		}, 5*time.Minute, 2*time.Second).Should(BeTrue())
		Expect(logs("valid")).To(ContainSubstring("installed verified " + pin.Integrity))
		assertGatewayPlugin()
	})
})
