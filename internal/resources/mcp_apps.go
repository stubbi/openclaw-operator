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

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// McpAppsSandbox describes how the operator exposes the OpenClaw MCP Apps
// sandbox listener (#615).
type McpAppsSandbox struct {
	// Exposed is true when the operator renders the sandbox ports (container
	// ports, nginx listener, Service port and NetworkPolicy ingress).
	Exposed bool
	// ListenPort is the port the OpenClaw sandbox listener binds to: the
	// user's mcp.apps.sandboxPort when set, otherwise McpAppsSandboxPort,
	// which config enrichment pins.
	ListenPort int32
}

// ResolveMcpAppsSandbox decides whether the MCP Apps sandbox is exposed and
// which port its listener uses.
//
// The operator can only see the inline config (spec.config.raw). When the
// config comes from spec.config.configMapRef, or mergeMode=merge lets runtime
// changes on the PVC enable MCP Apps, the operator cannot tell whether MCP
// Apps is on, so it keeps exposing the sandbox ports. Otherwise the ports are
// only rendered when the inline config sets mcp.apps.enabled=true, so
// instances that do not use MCP Apps see no pod template, Service or
// NetworkPolicy change.
func ResolveMcpAppsSandbox(instance *openclawv1alpha1.OpenClawInstance) McpAppsSandbox {
	enabled, port := mcpAppsFromRawConfig(instance)
	result := McpAppsSandbox{Exposed: enabled, ListenPort: McpAppsSandboxPort}
	if port > 0 {
		result.ListenPort = port
	}
	if instance.Spec.Config.ConfigMapRef != nil || instance.Spec.Config.MergeMode == ConfigMergeModeMerge {
		result.Exposed = true
	}
	return result
}

// mcpAppsFromRawConfig reports mcp.apps.enabled and mcp.apps.sandboxPort
// (0 when unset or not a valid port) from the inline config.
func mcpAppsFromRawConfig(instance *openclawv1alpha1.OpenClawInstance) (enabled bool, port int32) {
	if instance.Spec.Config.Raw == nil || len(instance.Spec.Config.Raw.Raw) == 0 {
		return false, 0
	}
	var config map[string]interface{}
	if err := json.Unmarshal(instance.Spec.Config.Raw.Raw, &config); err != nil {
		return false, 0
	}
	return mcpAppsSettings(config)
}

// mcpAppsSettings extracts mcp.apps.enabled and mcp.apps.sandboxPort from a
// parsed OpenClaw config.
func mcpAppsSettings(config map[string]interface{}) (enabled bool, port int32) {
	mcp, _ := config["mcp"].(map[string]interface{})
	if mcp == nil {
		return false, 0
	}
	apps, _ := mcp["apps"].(map[string]interface{})
	if apps == nil {
		return false, 0
	}
	enabled, _ = apps["enabled"].(bool)
	if p, ok := apps["sandboxPort"].(float64); ok && p >= 1 && p <= 65535 && p == float64(int32(p)) {
		port = int32(p)
	}
	return enabled, port
}

// mcpAppsListenPortFromConfig returns the sandbox listener port from the
// final (enriched) config bytes, falling back to fallback. Used for the nginx
// upstream so that a port set in a configMapRef config is honored too.
func mcpAppsListenPortFromConfig(configJSON []byte, fallback int32) int32 {
	var config map[string]interface{}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return fallback
	}
	if _, port := mcpAppsSettings(config); port > 0 {
		return port
	}
	return fallback
}
