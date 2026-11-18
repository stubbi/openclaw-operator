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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

var _ = Describe("Termination grace period CRD validation", func() {
	DescribeTable("defaults and preserves values through API admission", func(spec string, want int64) {
		instance := &unstructured.Unstructured{}
		Expect(instance.UnmarshalJSON([]byte(`{
			"apiVersion":"openclaw.rocks/v1alpha1",
			"kind":"OpenClawInstance",
			"metadata":{"name":"termination-grace-schema","namespace":"default"},
			"spec":` + spec + `}`))).To(Succeed())
		Expect(k8sClient.Create(ctx, instance, client.DryRunAll)).To(Succeed())
		grace, found, err := unstructured.NestedInt64(instance.Object, "spec", "terminationGracePeriodSeconds")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(grace).To(Equal(want))
	},
		Entry("omitted", `{}`, int64(360)),
		Entry("null", `{"terminationGracePeriodSeconds":null}`, int64(360)),
		Entry("immediate", `{"terminationGracePeriodSeconds":0}`, int64(0)),
		Entry("one second", `{"terminationGracePeriodSeconds":1}`, int64(1)),
		Entry("legacy grace", `{"terminationGracePeriodSeconds":30}`, int64(30)),
		Entry("longer grace", `{"terminationGracePeriodSeconds":600}`, int64(600)),
	)

	It("rejects negative grace periods on create and update without a webhook", func() {
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "termination-grace-invalid", Namespace: "default"},
			Spec: openclawv1alpha1.OpenClawInstanceSpec{
				Suspended:                     true,
				TerminationGracePeriodSeconds: resources.Ptr(int64(-1)),
			},
		}
		err := k8sClient.Create(ctx, instance, client.DryRunAll)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected API validation failure: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.terminationGracePeriodSeconds"))

		instance.Spec.TerminationGracePeriodSeconds = resources.Ptr(int64(0))
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, instance)).To(Succeed()) })
		Expect(instance.Spec.TerminationGracePeriodSeconds).To(Equal(resources.Ptr(int64(0))))
		err = k8sClient.Patch(ctx, instance, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"terminationGracePeriodSeconds":-1}}`)), client.DryRunAll)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected API validation failure: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.terminationGracePeriodSeconds"))
	})
})
