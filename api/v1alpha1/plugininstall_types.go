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

package v1alpha1

import corev1 "k8s.io/api/core/v1"

// PluginInstallSpec configures the managed installer for Plugins and VerifiedPlugins.
type PluginInstallSpec struct {
	// Resources sets requests and limits for the plugin installer and, when enabled,
	// its scratch-directory initializer. It does not change gateway resources.
	// Resource claims are not supported; only requests and limits may be set.
	// +kubebuilder:validation:XValidation:rule="!has(self.claims) || size(self.claims) == 0",message="plugin installer resource claims are not supported"
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// InheritEnv copies spec.env and spec.envFrom into the installer. Defaults to
	// true for compatibility. Set false to avoid exposing runtime credentials;
	// operator-managed HOME, npm settings and CA bundle configuration remain.
	// +optional
	InheritEnv *bool `json:"inheritEnv,omitempty"`
	// ReadOnlyRootFilesystem mounts the installer root filesystem read-only.
	// A non-root init container prepares a private 0700 scratch directory in an
	// emptyDir, mounted at /tmp via subPath. This avoids fsGroup's non-sticky
	// world-writable volume root. Persistent npm prefix/cache paths remain writable.
	// The emptyDir size limit follows resources.limits.ephemeral-storage when set,
	// otherwise 128Mi. Defaults to false, preserving the image's sticky /tmp.
	// +optional
	ReadOnlyRootFilesystem bool `json:"readOnlyRootFilesystem,omitempty"`
}
