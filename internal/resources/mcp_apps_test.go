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

package resources

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// These tests cover #615: the MCP Apps sandbox listener must not collide with
// the gateway proxy port and must be reachable through the proxy sidecar, the
// Service and the NetworkPolicy.

func TestMcpAppsSandboxPorts_DoNotCollide(t *testing.T) {
	seen := map[int]string{}
	for name, port := range map[string]int{
		"GatewayPort":             GatewayPort,
		"GatewayProxyPort":        GatewayProxyPort,
		"CanvasPort":              CanvasPort,
		"CanvasProxyPort":         CanvasProxyPort,
		"McpAppsSandboxPort":      McpAppsSandboxPort,
		"McpAppsSandboxProxyPort": McpAppsSandboxProxyPort,
	} {
		if other, ok := seen[port]; ok {
			t.Errorf("%s and %s both use port %d", name, other, port)
		}
		seen[port] = name
	}
	// OpenClaw's default sandbox port is "gateway port plus one". That is the
	// gateway proxy port, so the operator must never leave the default in place.
	if McpAppsSandboxPort == GatewayPort+1 {
		t.Errorf("McpAppsSandboxPort must not be gateway+1 (%d): nginx listens there", GatewayPort+1)
	}
}

func mcpAppsFromConfig(t *testing.T, instance *openclawv1alpha1.OpenClawInstance) map[string]interface{} {
	t.Helper()
	cm := BuildConfigMap(instance, "", nil)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cm.Data["openclaw.json"]), &parsed); err != nil {
		t.Fatalf("failed to parse config JSON: %v", err)
	}
	mcp, _ := parsed["mcp"].(map[string]interface{})
	if mcp == nil {
		return nil
	}
	apps, _ := mcp["apps"].(map[string]interface{})
	return apps
}

func TestBuildConfigMap_McpAppsSandboxPort(t *testing.T) {
	for _, tt := range []struct {
		name          string
		raw           string
		proxyDisabled bool
		wantApps      bool
		wantPort      interface{} // nil means the key must be absent
	}{
		{name: "no mcp section leaves config untouched", raw: `{}`},
		{name: "mcp without apps leaves config untouched", raw: `{"mcp":{"servers":{}}}`},
		{name: "apps disabled is not modified", raw: `{"mcp":{"apps":{"enabled":false}}}`, wantApps: true},
		{name: "apps enabled gets the operator port", raw: `{"mcp":{"apps":{"enabled":true}}}`, wantApps: true, wantPort: float64(McpAppsSandboxPort)},
		{name: "user port wins", raw: `{"mcp":{"apps":{"enabled":true,"sandboxPort":19001}}}`, wantApps: true, wantPort: float64(19001)},
		{name: "proxy disabled still pins the operator port", raw: `{"mcp":{"apps":{"enabled":true}}}`, proxyDisabled: true, wantApps: true, wantPort: float64(McpAppsSandboxPort)},
		{name: "proxy disabled keeps the user port", raw: `{"mcp":{"apps":{"enabled":true,"sandboxPort":19001}}}`, proxyDisabled: true, wantApps: true, wantPort: float64(19001)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance("mcp-apps")
			instance.Spec.Config.Raw = &openclawv1alpha1.RawConfig{
				RawExtension: runtime.RawExtension{Raw: []byte(tt.raw)},
			}
			if tt.proxyDisabled {
				instance.Spec.Gateway.Enabled = Ptr(false)
			}

			apps := mcpAppsFromConfig(t, instance)
			if (apps != nil) != tt.wantApps {
				t.Fatalf("mcp.apps present = %v, want %v", apps != nil, tt.wantApps)
			}
			if apps == nil {
				return
			}
			got, ok := apps["sandboxPort"]
			if tt.wantPort == nil {
				if ok {
					t.Errorf("mcp.apps.sandboxPort = %v, want it absent", got)
				}
				return
			}
			if got != tt.wantPort {
				t.Errorf("mcp.apps.sandboxPort = %v, want %v", got, tt.wantPort)
			}
		})
	}
}

func withRawConfig(instance *openclawv1alpha1.OpenClawInstance, raw string) *openclawv1alpha1.OpenClawInstance {
	instance.Spec.Config.Raw = &openclawv1alpha1.RawConfig{
		RawExtension: runtime.RawExtension{Raw: []byte(raw)},
	}
	return instance
}

const (
	mcpAppsEnabledRaw  = `{"mcp":{"apps":{"enabled":true}}}`
	mcpAppsUserPortRaw = `{"mcp":{"apps":{"enabled":true,"sandboxPort":19001}}}`
)

// nginxSandboxBlock returns the server block that listens on the sandbox proxy
// port, or "" when there is none.
func nginxSandboxBlock(conf string) string {
	listen := fmt.Sprintf("listen 0.0.0.0:%d;", McpAppsSandboxProxyPort)
	i := strings.Index(conf, listen)
	if i < 0 {
		return ""
	}
	block := conf[i:]
	if end := strings.Index(block, "}"); end >= 0 {
		block = block[:end]
	}
	return block
}

func TestResolveMcpAppsSandbox(t *testing.T) {
	for _, tt := range []struct {
		name        string
		raw         string
		configMap   bool
		merge       bool
		wantExposed bool
		wantPort    int32
	}{
		{name: "no config", wantPort: McpAppsSandboxPort},
		{name: "apps not configured", raw: `{"gateway":{}}`, wantPort: McpAppsSandboxPort},
		{name: "apps disabled", raw: `{"mcp":{"apps":{"enabled":false}}}`, wantPort: McpAppsSandboxPort},
		{name: "apps enabled", raw: mcpAppsEnabledRaw, wantExposed: true, wantPort: McpAppsSandboxPort},
		{name: "apps enabled with user port", raw: mcpAppsUserPortRaw, wantExposed: true, wantPort: 19001},
		{name: "invalid user port falls back", raw: `{"mcp":{"apps":{"enabled":true,"sandboxPort":70000}}}`, wantExposed: true, wantPort: McpAppsSandboxPort},
		{name: "configMapRef cannot be inspected", configMap: true, wantExposed: true, wantPort: McpAppsSandboxPort},
		{name: "mergeMode=merge may enable apps at runtime", raw: `{}`, merge: true, wantExposed: true, wantPort: McpAppsSandboxPort},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance("mcp-apps-resolve")
			if tt.raw != "" {
				withRawConfig(instance, tt.raw)
			}
			if tt.configMap {
				instance.Spec.Config.ConfigMapRef = &openclawv1alpha1.ConfigMapKeySelector{Name: "external"}
			}
			if tt.merge {
				instance.Spec.Config.MergeMode = ConfigMergeModeMerge
			}
			got := ResolveMcpAppsSandbox(instance)
			if got.Exposed != tt.wantExposed || got.ListenPort != tt.wantPort {
				t.Errorf("ResolveMcpAppsSandbox() = %+v, want exposed=%v port=%d", got, tt.wantExposed, tt.wantPort)
			}
		})
	}
}

func TestNginxStreamConfig_McpAppsSandbox(t *testing.T) {
	if block := nginxSandboxBlock(nginxStreamConfig(0)); block != "" {
		t.Errorf("sandbox server block rendered without an upstream:\n%s", block)
	}

	block := nginxSandboxBlock(nginxStreamConfig(McpAppsSandboxPort))
	if block == "" {
		t.Fatal("nginx config does not listen on the sandbox proxy port")
	}
	if pass := fmt.Sprintf("proxy_pass 127.0.0.1:%d;", McpAppsSandboxPort); !strings.Contains(block, pass) {
		t.Errorf("sandbox proxy server block does not forward to %d:\n%s", McpAppsSandboxPort, block)
	}
}

func TestBuildConfigMap_NginxMcpAppsUpstream(t *testing.T) {
	for _, tt := range []struct {
		name         string
		raw          string
		wantUpstream int32 // 0 means no sandbox server block
	}{
		{name: "apps not enabled", raw: `{}`},
		{name: "apps enabled", raw: mcpAppsEnabledRaw, wantUpstream: McpAppsSandboxPort},
		{name: "user port", raw: mcpAppsUserPortRaw, wantUpstream: 19001},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance := withRawConfig(newTestInstance("mcp-apps-nginx"), tt.raw)
			block := nginxSandboxBlock(BuildConfigMap(instance, "", nil).Data[NginxConfigKey])
			if tt.wantUpstream == 0 {
				if block != "" {
					t.Errorf("unexpected sandbox server block:\n%s", block)
				}
				return
			}
			if pass := fmt.Sprintf("proxy_pass 127.0.0.1:%d;", tt.wantUpstream); !strings.Contains(block, pass) {
				t.Errorf("sandbox server block does not forward to %d:\n%s", tt.wantUpstream, block)
			}
		})
	}

	// A port set in an external (configMapRef) config is honored by nginx.
	instance := newTestInstance("mcp-apps-nginx-cmref")
	instance.Spec.Config.ConfigMapRef = &openclawv1alpha1.ConfigMapKeySelector{Name: "external"}
	cm := BuildConfigMapFromBytes(instance, []byte(mcpAppsUserPortRaw), "", nil)
	if pass := "proxy_pass 127.0.0.1:19001;"; !strings.Contains(nginxSandboxBlock(cm.Data[NginxConfigKey]), pass) {
		t.Errorf("configMapRef sandbox port not used as nginx upstream:\n%s", cm.Data[NginxConfigKey])
	}
}

func containerByName(t *testing.T, instance *openclawv1alpha1.OpenClawInstance, name string) *corev1.Container {
	t.Helper()
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == name {
			return &sts.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

func hasContainerPortNamed(ports []corev1.ContainerPort, name string) bool {
	for _, p := range ports {
		if p.Name == name {
			return true
		}
	}
	return false
}

func TestBuildStatefulSet_McpAppsContainerPorts(t *testing.T) {
	instance := withRawConfig(newTestInstance("mcp-apps-sts"), mcpAppsEnabledRaw)
	main := containerByName(t, instance, "openclaw")
	proxy := containerByName(t, instance, "gateway-proxy")
	if main == nil || proxy == nil {
		t.Fatalf("expected openclaw and gateway-proxy containers")
	}
	assertContainerPort(t, main.Ports, "mcp-apps", McpAppsSandboxPort)
	assertContainerPort(t, proxy.Ports, "mcp-apps-proxy", McpAppsSandboxProxyPort)

	// User port is declared on the main container.
	withRawConfig(instance, mcpAppsUserPortRaw)
	assertContainerPort(t, containerByName(t, instance, "openclaw").Ports, "mcp-apps", 19001)
}

func TestBuildStatefulSet_McpAppsDisabledAddsNoPorts(t *testing.T) {
	instance := newTestInstance("mcp-apps-sts-off")
	if hasContainerPortNamed(containerByName(t, instance, "openclaw").Ports, "mcp-apps") {
		t.Error("main container declares mcp-apps without MCP Apps enabled")
	}
	if hasContainerPortNamed(containerByName(t, instance, "gateway-proxy").Ports, "mcp-apps-proxy") {
		t.Error("proxy container declares mcp-apps-proxy without MCP Apps enabled")
	}
}

func TestBuildService_McpAppsPort(t *testing.T) {
	instance := withRawConfig(newTestInstance("mcp-apps-svc"), mcpAppsEnabledRaw)
	assertServicePortWithTarget(t, BuildService(instance).Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), int32(McpAppsSandboxProxyPort))

	// Proxy disabled: target the listener directly (review on #617).
	instance.Spec.Gateway.Enabled = Ptr(false)
	assertServicePortWithTarget(t, BuildService(instance).Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), int32(McpAppsSandboxPort))

	// Proxy disabled with a user port: target the user port.
	withRawConfig(instance, mcpAppsUserPortRaw)
	assertServicePortWithTarget(t, BuildService(instance).Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), 19001)

	// Proxy enabled with a user port: still through nginx.
	instance.Spec.Gateway.Enabled = nil
	assertServicePortWithTarget(t, BuildService(instance).Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), int32(McpAppsSandboxProxyPort))
}

func TestBuildService_McpAppsDisabledAddsNoPort(t *testing.T) {
	for _, p := range BuildService(newTestInstance("mcp-apps-svc-off")).Spec.Ports {
		if p.Name == "mcp-apps" {
			t.Fatalf("Service has an mcp-apps port without MCP Apps enabled: %+v", p)
		}
	}
}

func TestBuildNetworkPolicy_McpAppsIngressPort(t *testing.T) {
	instance := withRawConfig(newTestInstance("mcp-apps-np"), mcpAppsEnabledRaw)
	assertNPPort(t, networkPolicyIngressPorts(instance), McpAppsSandboxProxyPort)

	instance.Spec.Gateway.Enabled = Ptr(false)
	assertNPPort(t, networkPolicyIngressPorts(instance), McpAppsSandboxPort)

	withRawConfig(instance, mcpAppsUserPortRaw)
	assertNPPort(t, networkPolicyIngressPorts(instance), 19001)
}

func TestBuildNetworkPolicy_McpAppsDisabledAddsNoPort(t *testing.T) {
	for _, p := range networkPolicyIngressPorts(newTestInstance("mcp-apps-np-off")) {
		if p.Port != nil && (p.Port.IntValue() == McpAppsSandboxProxyPort || p.Port.IntValue() == McpAppsSandboxPort) {
			t.Fatalf("NetworkPolicy allows the sandbox port without MCP Apps enabled: %+v", p)
		}
	}
}
