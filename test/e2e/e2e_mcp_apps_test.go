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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// This suite verifies the fix for #615: OpenClaw's MCP Apps sandbox listener
// defaults to "gateway port plus one", which is the port the gateway proxy
// sidecar already listens on, and it binds to loopback only. The operator pins
// the sandbox to its own port and forwards it through the proxy, the Service
// and the NetworkPolicy like the gateway and canvas ports.
var _ = Describe("MCP Apps sandbox port (#615)", func() {
	const (
		timeout  = time.Second * 60
		interval = time.Second * 1
	)

	var namespace string

	BeforeEach(func() {
		if os.Getenv("E2E_SKIP_RESOURCE_VALIDATION") == "true" {
			Skip("Skipping resource validation in minimal mode")
		}
		namespace = "test-mcp-apps-" + time.Now().Format("20060102150405")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	newInstance := func(name, raw string) *openclawv1alpha1.OpenClawInstance {
		return &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
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
				Config: openclawv1alpha1.ConfigSpec{
					Raw: &openclawv1alpha1.RawConfig{
						RawExtension: runtime.RawExtension{Raw: []byte(raw)},
					},
				},
			},
		}
	}

	getConfigMap := func(instance *openclawv1alpha1.OpenClawInstance) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{
				Name:      resources.ConfigMapName(instance),
				Namespace: namespace,
			}, cm)
		}, timeout, interval).Should(Succeed())
		return cm
	}

	getService := func(instance *openclawv1alpha1.OpenClawInstance) *corev1.Service {
		service := &corev1.Service{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{
				Name:      resources.ServiceName(instance),
				Namespace: namespace,
			}, service)
		}, timeout, interval).Should(Succeed())
		return service
	}

	findServicePort := func(service *corev1.Service, name string) *corev1.ServicePort {
		for i := range service.Spec.Ports {
			if service.Spec.Ports[i].Name == name {
				return &service.Spec.Ports[i]
			}
		}
		return nil
	}

	It("Should pin the sandbox port and expose it through the proxy and Service", func() {
		instance := newInstance("mcp-apps", `{"mcp":{"apps":{"enabled":true}}}`)
		Expect(k8sClient.Create(ctx, instance)).Should(Succeed())

		cm := getConfigMap(instance)

		var parsed map[string]interface{}
		Expect(json.Unmarshal([]byte(cm.Data["openclaw.json"]), &parsed)).To(Succeed())
		mcp, ok := parsed["mcp"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "rendered config should keep the mcp section")
		apps, ok := mcp["apps"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "rendered config should keep mcp.apps")
		Expect(apps["sandboxPort"]).To(BeEquivalentTo(resources.McpAppsSandboxPort),
			"the sandbox must not stay on gateway+1, which the gateway proxy occupies")

		nginxConf := cm.Data[resources.NginxConfigKey]
		Expect(nginxConf).To(ContainSubstring(fmt.Sprintf("listen 0.0.0.0:%d;", resources.McpAppsSandboxProxyPort)))
		Expect(nginxConf).To(ContainSubstring(fmt.Sprintf("proxy_pass 127.0.0.1:%d;", resources.McpAppsSandboxPort)))
		Expect(strings.Count(nginxConf, "listen 0.0.0.0:")).To(Equal(3),
			"gateway, canvas and MCP Apps sandbox should each have one proxy listener")

		p := findServicePort(getService(instance), "mcp-apps")
		Expect(p).NotTo(BeNil(), "Service should expose the mcp-apps port")
		Expect(p.Port).To(BeEquivalentTo(resources.McpAppsSandboxPort))
		Expect(p.TargetPort.IntValue()).To(Equal(resources.McpAppsSandboxProxyPort),
			"the Service must target the proxy port because the sandbox binds to loopback")
	})

	It("Should target the user's sandbox port directly when the gateway proxy is disabled", func() {
		instance := newInstance("mcp-apps-noproxy", `{"mcp":{"apps":{"enabled":true,"sandboxPort":19001}}}`)
		instance.Spec.Gateway.Enabled = resources.Ptr(false)
		Expect(k8sClient.Create(ctx, instance)).Should(Succeed())

		cm := getConfigMap(instance)
		var parsed map[string]interface{}
		Expect(json.Unmarshal([]byte(cm.Data["openclaw.json"]), &parsed)).To(Succeed())
		apps := parsed["mcp"].(map[string]interface{})["apps"].(map[string]interface{})
		Expect(apps["sandboxPort"]).To(BeEquivalentTo(19001), "a user-supplied sandbox port must win")
		Expect(cm.Data).NotTo(HaveKey(resources.NginxConfigKey), "no proxy config without the proxy sidecar")

		var p *corev1.ServicePort
		Eventually(func() *corev1.ServicePort {
			p = findServicePort(getService(instance), "mcp-apps")
			return p
		}, timeout, interval).ShouldNot(BeNil(), "Service should expose the mcp-apps port")
		Expect(p.Port).To(BeEquivalentTo(resources.McpAppsSandboxPort))
		Expect(p.TargetPort.IntValue()).To(Equal(19001),
			"without the proxy the Service must target the effective sandbox listener port")
	})

	It("Should not expose the sandbox when MCP Apps is not enabled", func() {
		instance := newInstance("mcp-apps-off", `{}`)
		Expect(k8sClient.Create(ctx, instance)).Should(Succeed())

		cm := getConfigMap(instance)
		nginxConf := cm.Data[resources.NginxConfigKey]
		Expect(nginxConf).NotTo(ContainSubstring(fmt.Sprintf("listen 0.0.0.0:%d;", resources.McpAppsSandboxProxyPort)))
		Expect(strings.Count(nginxConf, "listen 0.0.0.0:")).To(Equal(2),
			"only gateway and canvas should have proxy listeners")

		Expect(findServicePort(getService(instance), "mcp-apps")).To(BeNil(),
			"Service must not gain an mcp-apps port for instances that do not use MCP Apps")
	})
})
