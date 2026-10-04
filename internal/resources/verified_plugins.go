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
	_ "embed"
	"encoding/json"
	"sort"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// Keep the installer as an executable module so tests exercise the exact code
// embedded in the operator, without a separate installer image or shell quoting.
//
//go:embed scripts/install-verified-plugins.mjs
var verifiedPluginsInstaller string

func hasPlugins(instance *openclawv1alpha1.OpenClawInstance) bool {
	return len(instance.Spec.Plugins) > 0 || len(instance.Spec.VerifiedPlugins) > 0
}

func verifiedPluginArgs(instance *openclawv1alpha1.OpenClawInstance) []string {
	pins := append([]openclawv1alpha1.VerifiedPluginSpec(nil), instance.Spec.VerifiedPlugins...)
	sort.Slice(pins, func(i, j int) bool { return pins[i].Package < pins[j].Package })
	data, _ := json.Marshal(pins)
	return []string{string(data)}
}
