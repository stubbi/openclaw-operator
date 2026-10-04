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
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

func TestPluginInstallControls(t *testing.T) {
	for _, verified := range []bool{false, true} {
		name := "legacy"
		if verified {
			name = "verified"
		}
		t.Run(name, func(t *testing.T) {
			instance := newTestInstance("controls")
			if verified {
				instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{{Package: "example", Version: "1.2.3", Integrity: "sha512-pin"}}
			} else {
				instance.Spec.Plugins = []string{"npm:example@1.2.3"}
			}
			instance.Spec.Env = []corev1.EnvVar{{Name: "RUNTIME_SECRET", Value: "never-pass-to-installer"}}
			instance.Spec.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-secrets"}}}}
			instance.Spec.Security.CABundle = &openclawv1alpha1.CABundleSpec{ConfigMapName: "registry-ca"}
			original := buildPluginsInitContainer(instance)
			if len(original.EnvFrom) != 1 || *original.SecurityContext.ReadOnlyRootFilesystem {
				t.Fatal("defaults must retain existing environment and writable root")
			}
			for _, readOnly := range []bool{false, true} {
				instance.Spec.PluginInstall = &openclawv1alpha1.PluginInstallSpec{
					InheritEnv: Ptr(false), ReadOnlyRootFilesystem: readOnly,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("96Mi")},
					},
				}
				// Exercise matching identities for the scratch helper and installer even
				// when the gateway overrides the pod UID.
				instance.Spec.Security.ContainerSecurityContext = &openclawv1alpha1.ContainerSecurityContextSpec{RunAsUser: Ptr(int64(2000))}
				sts := BuildStatefulSet(instance, "", nil, nil, nil)
				var scratch, installer *corev1.Container
				var scratchIndex, installerIndex int
				for i := range sts.Spec.Template.Spec.InitContainers {
					c := &sts.Spec.Template.Spec.InitContainers[i]
					if c.Name == "init-plugin-scratch" {
						scratch = c
						scratchIndex = i
					}
					if c.Name == "init-plugins" {
						installer = c
						installerIndex = i
					}
				}
				if installer == nil {
					t.Fatal("missing installer")
				}
				if len(installer.EnvFrom) != 0 {
					t.Fatal("runtime envFrom leaked")
				}
				env := map[string]string{}
				for _, e := range installer.Env {
					env[e.Name] = e.Value
				}
				if _, ok := env["RUNTIME_SECRET"]; ok {
					t.Fatal("runtime env leaked")
				}
				if env["HOME"] == "" || env["NODE_EXTRA_CA_CERTS"] == "" || env["NPM_CONFIG_IGNORE_SCRIPTS"] != "true" {
					t.Fatalf("lost operator environment: %v", env)
				}
				if !reflect.DeepEqual(installer.Resources, instance.Spec.PluginInstall.Resources) {
					t.Fatal("installer resources changed")
				}
				if *installer.SecurityContext.ReadOnlyRootFilesystem != readOnly {
					t.Fatal("wrong root filesystem policy")
				}
				if readOnly {
					if scratch == nil || scratchIndex >= installerIndex {
						t.Fatal("scratch must be initialized first")
					}
					if len(scratch.EnvFrom) != 0 || len(scratch.Env) != 0 {
						t.Fatal("scratch helper must receive no runtime environment")
					}
					if *scratch.SecurityContext.RunAsUser != 2000 || *installer.SecurityContext.RunAsUser != 2000 {
						t.Fatal("scratch and installer must share effective UID")
					}
					if !*scratch.SecurityContext.ReadOnlyRootFilesystem || *scratch.SecurityContext.AllowPrivilegeEscalation || len(scratch.SecurityContext.Capabilities.Add) != 0 {
						t.Fatal("scratch helper must stay restricted")
					}
					if scratch.Image != installer.Image || !reflect.DeepEqual(scratch.Resources, installer.Resources) {
						t.Fatal("scratch helper must reuse pinned image and limits")
					}
					found := false
					for _, mount := range installer.VolumeMounts {
						if mount.MountPath == "/tmp" {
							found = true
							if mount.Name != pluginScratchVolume || mount.SubPath != "private" {
								t.Fatal("must mount only the private scratch directory")
							}
						}
					}
					if !found {
						t.Fatal("missing scratch mount")
					}
					volume := findVolume(sts.Spec.Template.Spec.Volumes, pluginScratchVolume)
					if volume == nil || volume.EmptyDir.SizeLimit.Cmp(resource.MustParse("96Mi")) != 0 {
						t.Fatal("scratch volume must be bounded")
					}
				} else if scratch != nil || findVolume(sts.Spec.Template.Spec.Volumes, pluginScratchVolume) != nil {
					t.Fatal("writable image must retain its own sticky /tmp")
				}
				// Turning off inheritance applies only to the installer, not the gateway.
				main := buildMainContainer(instance, "")
				if !reflect.DeepEqual(main.EnvFrom, instance.Spec.EnvFrom) {
					t.Fatal("gateway lost environment")
				}
			}
			copied := instance.DeepCopy()
			copied.Spec.PluginInstall.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
			if instance.Spec.PluginInstall.Resources.Limits.Memory().Cmp(resource.MustParse("1Gi")) != 0 {
				t.Fatal("deep copy aliases resources")
			}
		})
	}
}

func TestPluginInstallWithoutPlugins(t *testing.T) {
	instance := newTestInstance("no-plugins")
	instance.Spec.PluginInstall = &openclawv1alpha1.PluginInstallSpec{ReadOnlyRootFilesystem: true}
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	for _, c := range sts.Spec.Template.Spec.InitContainers {
		if c.Name == "init-plugins" || c.Name == "init-plugin-scratch" {
			t.Fatal("plugin controls alone must not create containers")
		}
	}
	if findVolume(sts.Spec.Template.Spec.Volumes, pluginScratchVolume) != nil {
		t.Fatal("unused scratch volume")
	}
}

func TestPluginInstallNonRootPolicy(t *testing.T) {
	for _, verified := range []bool{false, true} {
		for _, readOnly := range []bool{false, true} {
			for _, policy := range []struct {
				name    string
				pod     *bool
				gateway bool
				want    bool
			}{
				{name: "default pod policy", gateway: false, want: true},
				{name: "explicit non-root pod", pod: Ptr(true), gateway: false, want: true},
				{name: "pod permits root", pod: Ptr(false), gateway: true, want: false},
			} {
				t.Run(fmt.Sprintf("verified=%t/readOnly=%t/%s", verified, readOnly, policy.name), func(t *testing.T) {
					instance := newTestInstance("non-root-policy")
					if verified {
						instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{{Package: "example", Version: "1.2.3", Integrity: "sha512-pin"}}
					} else {
						instance.Spec.Plugins = []string{"npm:example@1.2.3"}
					}
					if readOnly {
						instance.Spec.PluginInstall = &openclawv1alpha1.PluginInstallSpec{ReadOnlyRootFilesystem: true}
					}
					instance.Spec.Security.PodSecurityContext = &openclawv1alpha1.PodSecurityContextSpec{RunAsNonRoot: policy.pod}
					instance.Spec.Security.ContainerSecurityContext = &openclawv1alpha1.ContainerSecurityContextSpec{
						RunAsUser: Ptr(int64(2000)), RunAsNonRoot: Ptr(policy.gateway),
					}
					sts := BuildStatefulSet(instance, "", nil, nil, nil)
					checked := 0
					for _, c := range sts.Spec.Template.Spec.InitContainers {
						if c.Name != "init-plugins" && c.Name != "init-plugin-scratch" {
							continue
						}
						checked++
						if c.SecurityContext.RunAsNonRoot == nil || *c.SecurityContext.RunAsNonRoot != policy.want {
							t.Errorf("%s must retain pod non-root policy %t", c.Name, policy.want)
						}
						if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 2000 {
							t.Errorf("%s must use gateway UID", c.Name)
						}
					}
					wantContainers := 1
					if readOnly {
						wantContainers++
					}
					if checked != wantContainers {
						t.Fatalf("checked %d containers, want %d", checked, wantContainers)
					}
					main := buildMainContainer(instance, "")
					if *main.SecurityContext.RunAsNonRoot != policy.gateway {
						t.Fatal("gateway must retain its own non-root policy")
					}
				})
			}
		}
	}
}
