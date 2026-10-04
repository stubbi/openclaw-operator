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

package webhook

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

func TestVerifiedPluginsAdmission(t *testing.T) {
	pin := openclawv1alpha1.VerifiedPluginSpec{
		Package: "@third-party/plugin", Version: "1.2.3-rc.1+build.5",
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
	tests := []struct {
		name   string
		mutate func(*openclawv1alpha1.OpenClawInstanceSpec)
		want   string
	}{
		{"valid", func(s *openclawv1alpha1.OpenClawInstanceSpec) {}, ""},
		{"consent", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].AcceptCapabilities = true }, ""},
		{"legacy mixed", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.Plugins = []string{"other"} }, "mutually exclusive"},
		{"prefix", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Package = "npm:plugin" }, ".package"},
		{"traversal", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Package = "../plugin" }, ".package"},
		{"version suffix", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Package += "@1.2.3" }, ".package"},
		{"range", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Version = "^1.2.3" }, ".version"},
		{"tag", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Version = "latest" }, ".version"},
		{"leading v", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Version = "v1.2.3" }, ".version"},
		{"leading zero", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Version = "1.2.3-01" }, ".version"},
		{"digest", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins[0].Integrity = "sha512-invalid" }, ".integrity"},
		{"noncanonical digest", func(s *openclawv1alpha1.OpenClawInstanceSpec) {
			s.VerifiedPlugins[0].Integrity = "sha512-" + strings.Repeat("A", 85) + "B=="
		}, ".integrity"},
		{"duplicate", func(s *openclawv1alpha1.OpenClawInstanceSpec) { s.VerifiedPlugins = append(s.VerifiedPlugins, pin) }, "duplicates"},
		{"directory collision", func(s *openclawv1alpha1.OpenClawInstanceSpec) {
			other := pin
			other.Package = "plugin"
			s.VerifiedPlugins = append(s.VerifiedPlugins, other)
		}, "duplicates"},
		{"too many", func(s *openclawv1alpha1.OpenClawInstanceSpec) {
			s.VerifiedPlugins = make([]openclawv1alpha1.VerifiedPluginSpec, 21)
		}, "at most 20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance()
			instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{pin}
			tt.mutate(&instance.Spec)
			v := &OpenClawInstanceValidator{}
			_, createErr := v.ValidateCreate(context.Background(), instance)
			_, updateErr := v.ValidateUpdate(context.Background(), newTestInstance(), instance)
			for _, err := range []error{createErr, updateErr} {
				if tt.want == "" && err != nil {
					t.Fatal(err)
				}
				if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
					t.Fatalf("want %q, got %v", tt.want, err)
				}
			}
		})
	}
}
