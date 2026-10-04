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

package controller

import (
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// Exercise the generated schema with a real API server, including when the
// optional validating webhook is not installed.
var _ = Describe("Verified plugin CRD validation", func() {
	DescribeTable("validates declarative pins", func(version, integrity string, legacy, duplicate, valid bool) {
		pin := openclawv1alpha1.VerifiedPluginSpec{
			Package: "@example/plugin", Version: version, Integrity: integrity,
		}
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "verified-schema", Namespace: "default"},
			Spec:       openclawv1alpha1.OpenClawInstanceSpec{VerifiedPlugins: []openclawv1alpha1.VerifiedPluginSpec{pin}},
		}
		if legacy {
			instance.Spec.Plugins = []string{"other"}
		}
		if duplicate {
			instance.Spec.VerifiedPlugins = append(instance.Spec.VerifiedPlugins, pin)
		}
		err := k8sClient.Create(ctx, instance, client.DryRunAll)
		if valid {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(HaveOccurred())
		}
	},
		Entry("exact version", "1.2.3", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), false, false, true),
		Entry("prerelease and build", "1.2.3-rc.1+build.5", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), false, false, true),
		Entry("range", "^1.2.3", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), false, false, false),
		Entry("leading zero", "1.2.3-01", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), false, false, false),
		Entry("malformed hash", "1.2.3", "sha512-invalid", false, false, false),
		Entry("mixed APIs", "1.2.3", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), true, false, false),
		Entry("duplicate package", "1.2.3", "sha512-"+base64.StdEncoding.EncodeToString(make([]byte, 64)), false, true, false),
	)
})
