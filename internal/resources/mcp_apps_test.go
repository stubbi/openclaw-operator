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
		{name: "proxy disabled keeps the OpenClaw default", raw: `{"mcp":{"apps":{"enabled":true}}}`, proxyDisabled: true, wantApps: true},
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

func TestNginxStreamConfig_ProxiesMcpAppsSandbox(t *testing.T) {
	conf := nginxStreamConfig()
	listen := fmt.Sprintf("listen 0.0.0.0:%d;", McpAppsSandboxProxyPort)
	pass := fmt.Sprintf("proxy_pass 127.0.0.1:%d;", McpAppsSandboxPort)
	i := strings.Index(conf, listen)
	if i < 0 {
		t.Fatalf("nginx config does not listen on the sandbox proxy port:\n%s", conf)
	}
	// The proxy_pass must belong to the same server block as the listen line.
	block := conf[i:]
	if end := strings.Index(block, "}"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(block, pass) {
		t.Errorf("sandbox proxy server block does not forward to %d:\n%s", McpAppsSandboxPort, block)
	}
}

func TestBuildStatefulSet_McpAppsContainerPorts(t *testing.T) {
	instance := newTestInstance("mcp-apps-sts")
	sts := BuildStatefulSet(instance, "", nil, nil, nil)

	var main, proxy bool
	for _, c := range sts.Spec.Template.Spec.Containers {
		switch c.Name {
		case "openclaw":
			main = true
			assertContainerPort(t, c.Ports, "mcp-apps", McpAppsSandboxPort)
		case "gateway-proxy":
			proxy = true
			assertContainerPort(t, c.Ports, "mcp-apps-proxy", McpAppsSandboxProxyPort)
		}
	}
	if !main || !proxy {
		t.Fatalf("expected openclaw and gateway-proxy containers, got main=%v proxy=%v", main, proxy)
	}
}

func TestBuildService_McpAppsPort(t *testing.T) {
	instance := newTestInstance("mcp-apps-svc")
	svc := BuildService(instance)
	assertServicePortWithTarget(t, svc.Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), int32(McpAppsSandboxProxyPort))

	instance.Spec.Gateway.Enabled = Ptr(false)
	svc = BuildService(instance)
	assertServicePortWithTarget(t, svc.Spec.Ports, "mcp-apps", int32(McpAppsSandboxPort), int32(McpAppsSandboxPort))
}

func TestBuildNetworkPolicy_McpAppsIngressPort(t *testing.T) {
	instance := newTestInstance("mcp-apps-np")
	assertNPPort(t, networkPolicyIngressPorts(instance), McpAppsSandboxProxyPort)

	instance.Spec.Gateway.Enabled = Ptr(false)
	assertNPPort(t, networkPolicyIngressPorts(instance), McpAppsSandboxPort)
}
