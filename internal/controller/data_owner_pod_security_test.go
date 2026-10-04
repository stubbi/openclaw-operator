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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// The init-data-owner init container (#607) runs as root with CAP_CHOWN. A
// namespace enforcing the "restricted" Pod Security Standard rejects such a
// pod at admission, so with storage.fixOwnership unset the operator must leave
// it out there, while an explicit value always wins.
var _ = Describe("init-data-owner and Pod Security Standards", func() {
	const (
		timeout  = time.Second * 30
		interval = time.Millisecond * 250
	)

	var counter int

	newNamespace := func(enforce string) string {
		counter++
		name := fmt.Sprintf("data-owner-pss-%d-%d", time.Now().UnixNano(), counter)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if enforce != "" {
			ns.Labels = map[string]string{resources.PodSecurityEnforceLabel: enforce}
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		return name
	}

	// initContainerNames creates an instance and returns the init container
	// names of the StatefulSet the operator renders for it.
	initContainerNames := func(namespace string, fixOwnership *bool) []string {
		instance := &openclawv1alpha1.OpenClawInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "data-owner", Namespace: namespace},
			Spec: openclawv1alpha1.OpenClawInstanceSpec{
				Storage: openclawv1alpha1.StorageSpec{FixOwnership: fixOwnership},
			},
		}
		instance.Spec.Config.Raw = &openclawv1alpha1.RawConfig{
			RawExtension: runtime.RawExtension{Raw: []byte(`{}`)},
		}
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())

		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{
				Name:      resources.StatefulSetName(instance),
				Namespace: namespace,
			}, sts)
		}, timeout, interval).Should(Succeed())

		names := make([]string, 0, len(sts.Spec.Template.Spec.InitContainers))
		for _, c := range sts.Spec.Template.Spec.InitContainers {
			names = append(names, c.Name)
		}
		return names
	}

	It("runs init-data-owner by default in a namespace without Pod Security enforcement", func() {
		names := initContainerNames(newNamespace(""), nil)
		Expect(names).To(ContainElement(resources.DataOwnerInitContainerName))
		Expect(names[0]).To(Equal(resources.DataOwnerInitContainerName))
	})

	It("runs init-data-owner by default in a namespace enforcing baseline", func() {
		// baseline permits root containers and the CHOWN capability.
		Expect(initContainerNames(newNamespace("baseline"), nil)).
			To(ContainElement(resources.DataOwnerInitContainerName))
	})

	It("leaves init-data-owner out by default in a namespace enforcing restricted", func() {
		Expect(initContainerNames(newNamespace("restricted"), nil)).
			NotTo(ContainElement(resources.DataOwnerInitContainerName))
	})

	It("honors an explicit fixOwnership=true in a namespace enforcing restricted", func() {
		Expect(initContainerNames(newNamespace("restricted"), resources.Ptr(true))).
			To(ContainElement(resources.DataOwnerInitContainerName))
	})

	It("honors an explicit fixOwnership=false in an unrestricted namespace", func() {
		Expect(initContainerNames(newNamespace(""), resources.Ptr(false))).
			NotTo(ContainElement(resources.DataOwnerInitContainerName))
	})

	It("does not write the resolved default back to the stored spec", func() {
		namespace := newNamespace("restricted")
		Expect(initContainerNames(namespace, nil)).
			NotTo(ContainElement(resources.DataOwnerInitContainerName))

		stored := &openclawv1alpha1.OpenClawInstance{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "data-owner", Namespace: namespace}, stored)).To(Succeed())
		Expect(stored.Spec.Storage.FixOwnership).To(BeNil(),
			"the namespace-derived default must stay in memory so relabeling the namespace takes effect")
	})
})
