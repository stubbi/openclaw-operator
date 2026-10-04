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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// Exercise the generated schema with a real API server, including when the
// optional validating webhook is not installed.
var _ = Describe("Verified plugin CRD validation", func() {
	It("rejects unsupported installer resource claims on create and update", func() {
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "installer-claims-schema", Namespace: "default"},
			Spec: openclawv1alpha1.OpenClawInstanceSpec{
				Plugins: []string{"npm:example@1.2.3"},
				PluginInstall: &openclawv1alpha1.PluginInstallSpec{
					Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "unsupported"}}},
				},
			},
		}
		createErr := k8sClient.Create(ctx, instance, client.DryRunAll)
		Expect(apierrors.IsInvalid(createErr)).To(BeTrue(), "expected resource claims to fail API validation: %v", createErr)
		Expect(createErr.Error()).To(ContainSubstring("plugin installer resource claims are not supported"))

		instance.Spec.PluginInstall.Resources.Claims = nil
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, instance)).To(Succeed()) })
		instance.Spec.PluginInstall.Resources.Claims = []corev1.ResourceClaim{{Name: "unsupported"}}
		updateErr := k8sClient.Update(ctx, instance, client.DryRunAll)
		Expect(apierrors.IsInvalid(updateErr)).To(BeTrue(), "expected resource claims to fail API validation: %v", updateErr)
		Expect(updateErr.Error()).To(ContainSubstring("plugin installer resource claims are not supported"))
	})

	It("preserves installer controls through API admission", func() {
		inherit := false
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "installer-schema", Namespace: "default"},
			Spec: openclawv1alpha1.OpenClawInstanceSpec{
				Plugins: []string{"npm:example@1.2.3"},
				PluginInstall: &openclawv1alpha1.PluginInstallSpec{
					InheritEnv: &inherit, ReadOnlyRootFilesystem: true,
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, instance, client.DryRunAll)).To(Succeed())
		Expect(instance.Spec.PluginInstall).NotTo(BeNil())
		Expect(instance.Spec.PluginInstall.InheritEnv).To(Equal(&inherit))
		Expect(instance.Spec.PluginInstall.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(instance.Spec.PluginInstall.Resources.Limits.Memory().Cmp(resource.MustParse("1Gi"))).To(BeZero())
	})

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
