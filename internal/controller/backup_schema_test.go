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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

var _ = Describe("Backup controls CRD validation", func() {
	It("preserves controls and supplies defaults through API admission", func() {
		instance := &openclawv1alpha1.OpenClawInstance{ObjectMeta: metav1.ObjectMeta{Name: "backup-schema", Namespace: "default"}}
		Expect(k8sClient.Create(ctx, instance, client.DryRunAll)).To(Succeed())
		Expect(instance.Spec.AutoUpdate.BackupTimeout).To(Equal("30m"))
		Expect(instance.Spec.Backup.Transfers).To(Equal(resources.Ptr(int32(8))))
		Expect(instance.Spec.Backup.Checkers).To(Equal(resources.Ptr(int32(16))))
		instance.ResourceVersion = ""
		instance.Spec.Backup.Excludes = []string{"/npm/**"}
		instance.Spec.Backup.Transfers = resources.Ptr(int32(32))
		instance.Spec.Backup.Checkers = resources.Ptr(int32(64))
		instance.Spec.Backup.FastList = true
		instance.Spec.AutoUpdate.BackupTimeout = "15m"
		Expect(k8sClient.Create(ctx, instance, client.DryRunAll)).To(Succeed())
		Expect(instance.Spec.Backup.Excludes).To(Equal([]string{"/npm/**"}))
		Expect(instance.Spec.Backup.Transfers).To(Equal(resources.Ptr(int32(32))))
		Expect(instance.Spec.Backup.Checkers).To(Equal(resources.Ptr(int32(64))))
		Expect(instance.Spec.Backup.FastList).To(BeTrue())
		Expect(instance.Spec.AutoUpdate.BackupTimeout).To(Equal("15m"))
	})
	DescribeTable("rejects invalid backup controls", func(spec openclawv1alpha1.BackupSpec) {
		instance := &openclawv1alpha1.OpenClawInstance{ObjectMeta: metav1.ObjectMeta{Name: "backup-invalid", Namespace: "default"}, Spec: openclawv1alpha1.OpenClawInstanceSpec{Backup: spec}}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, instance, client.DryRunAll))).To(BeTrue())
	},
		Entry("zero transfers", openclawv1alpha1.BackupSpec{Transfers: resources.Ptr(int32(0))}),
		Entry("too many checkers", openclawv1alpha1.BackupSpec{Checkers: resources.Ptr(int32(257))}),
		Entry("empty filter", openclawv1alpha1.BackupSpec{Excludes: []string{""}}),
		Entry("oversized filter", openclawv1alpha1.BackupSpec{Excludes: []string{strings.Repeat("x", 1025)}}),
	)
})
