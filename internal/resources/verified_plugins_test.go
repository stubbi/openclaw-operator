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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

func TestVerifiedPluginsPod(t *testing.T) {
	instance := newTestInstance("verified")
	instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{{Package: "example", Version: "1.2.3", Integrity: "sha512-pin", AcceptCapabilities: true}}
	instance.Spec.Security.CABundle = &openclawv1alpha1.CABundleSpec{ConfigMapName: "registry-ca"}
	instance.Spec.Env = []corev1.EnvVar{{Name: "EXAMPLE", Value: "present"}}
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	var installer *corev1.Container
	for i := range sts.Spec.Template.Spec.InitContainers {
		c := &sts.Spec.Template.Spec.InitContainers[i]
		if c.Name == "init-plugins" {
			installer = c
		}
	}
	if installer == nil {
		t.Fatal("missing plugin init container")
	}
	if installer.Image != GetImage(instance) {
		t.Fatal("installer image must match runtime")
	}
	if !reflect.DeepEqual(installer.Command, []string{"node", "--input-type=module", "--eval", verifiedPluginsInstaller}) {
		t.Fatal("installer must execute embedded module directly")
	}
	var pins []openclawv1alpha1.VerifiedPluginSpec
	if err := json.Unmarshal([]byte(installer.Args[0]), &pins); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pins, instance.Spec.VerifiedPlugins) {
		t.Fatalf("pins changed: %#v", pins)
	}
	for _, mount := range []struct{ name, path string }{{"data", "/home/openclaw/.openclaw"}, {"ca-bundle", "/etc/ssl/certs/custom-ca-bundle.crt"}} {
		assertVolumeMount(t, installer.VolumeMounts, mount.name, mount.path)
	}
	for _, mount := range installer.VolumeMounts {
		if mount.MountPath == "/tmp" {
			t.Fatal("installer must use the image's sticky /tmp directory")
		}
	}
	if findVolume(sts.Spec.Template.Spec.Volumes, "plugins-tmp") != nil {
		t.Fatal("unexpected plugins-tmp volume")
	}
	env := map[string]string{}
	for _, e := range installer.Env {
		env[e.Name] = e.Value
	}
	if env["NPM_CONFIG_IGNORE_SCRIPTS"] != "true" || env["NODE_EXTRA_CA_CERTS"] == "" || env["EXAMPLE"] != "present" {
		t.Fatalf("missing installer environment: %v", env)
	}
}

func TestVerifiedPluginsRollout(t *testing.T) {
	instance := newTestInstance("verified-hash")
	before := calculateConfigHash(instance, nil, nil, nil)
	instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{{Package: "example", Version: "1.2.3", Integrity: "sha512-pin"}}
	withPin := calculateConfigHash(instance, nil, nil, nil)
	if before == withPin {
		t.Fatal("adding a pin must roll out")
	}
	for _, mutate := range []func(*openclawv1alpha1.VerifiedPluginSpec){
		func(p *openclawv1alpha1.VerifiedPluginSpec) { p.Version = "1.2.4" },
		func(p *openclawv1alpha1.VerifiedPluginSpec) { p.Integrity = "sha512-new" },
		func(p *openclawv1alpha1.VerifiedPluginSpec) { p.AcceptCapabilities = true },
	} {
		updated := instance.DeepCopy()
		mutate(&updated.Spec.VerifiedPlugins[0])
		if calculateConfigHash(updated, nil, nil, nil) == withPin {
			t.Fatal("pin/consent change must roll out")
		}
		if instance.Spec.VerifiedPlugins[0].Version != "1.2.3" {
			t.Fatal("deep copy aliases pins")
		}
	}
}

func TestVerifiedPluginsDeterministic(t *testing.T) {
	instance := newTestInstance("verified-order")
	instance.Spec.VerifiedPlugins = []openclawv1alpha1.VerifiedPluginSpec{{Package: "z"}, {Package: "a"}}
	first := verifiedPluginArgs(instance)
	before := calculateConfigHash(instance, nil, nil, nil)
	if instance.Spec.VerifiedPlugins[0].Package != "z" {
		t.Fatal("sorting must not mutate the instance")
	}
	instance.Spec.VerifiedPlugins[0], instance.Spec.VerifiedPlugins[1] = instance.Spec.VerifiedPlugins[1], instance.Spec.VerifiedPlugins[0]
	if !reflect.DeepEqual(first, verifiedPluginArgs(instance)) || before != calculateConfigHash(instance, nil, nil, nil) {
		t.Fatal("list ordering must not change install order or rollout hash")
	}
}
