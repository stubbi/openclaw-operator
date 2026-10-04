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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

// Regression tests for #607: the data volume root is owned by root on
// fsGroup-only PVCs, and OpenClaw >= 2026.9 fails with
// "EPERM: operation not permitted, fchmod" when it tightens directory modes
// on ~/.openclaw. The init-data-owner init container chowns the volume root
// to the pod UID before anything else runs.

func findDataOwnerInitContainer(t *testing.T, containers []corev1.Container) *corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == DataOwnerInitContainerName {
			return &containers[i]
		}
	}
	return nil
}

func TestBuildStatefulSet_DataOwnerInitContainer_DefaultOnAndFirst(t *testing.T) {
	instance := newTestInstance("data-owner-default")

	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	initContainers := sts.Spec.Template.Spec.InitContainers

	if len(initContainers) == 0 {
		t.Fatal("expected init containers")
	}
	if initContainers[0].Name != DataOwnerInitContainerName {
		t.Fatalf("initContainers[0] = %q, want %q (ownership fix must run before any container writes to the data volume)",
			initContainers[0].Name, DataOwnerInitContainerName)
	}

	c := initContainers[0]
	if c.Image != "docker.io/library/busybox:1.37" {
		t.Errorf("image = %q, want docker.io/library/busybox:1.37", c.Image)
	}
	assertVolumeMount(t, c.VolumeMounts, "data", "/data")
	for _, m := range c.VolumeMounts {
		if m.Name == "data" && m.SubPath != "" {
			t.Errorf("data mount must target the volume root, got SubPath %q", m.SubPath)
		}
	}

	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" {
		t.Fatalf("command should be sh -c <script>, got %v", c.Command)
	}
	script := c.Command[2]
	for _, want := range []string{`want="1000:1000"`, `chown "$want" /data`, `exit 0`} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// chown must be skipped when ownership is already correct (idempotent on every boot)
	if !strings.Contains(script, `if [ "$have" = "$want" ]`) {
		t.Errorf("script should short-circuit when ownership already matches:\n%s", script)
	}
}

func TestBuildStatefulSet_DataOwnerInitContainer_SecurityContext(t *testing.T) {
	instance := newTestInstance("data-owner-sc")

	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	c := findDataOwnerInitContainer(t, sts.Spec.Template.Spec.InitContainers)
	if c == nil {
		t.Fatal("init-data-owner container not found")
	}
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("security context is nil")
	}

	// Must run as root: chown on a root-owned directory needs CAP_CHOWN, which
	// is only effective for uid 0 under the default capability bounding set.
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("runAsUser = %v, want 0", sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
		t.Errorf("runAsNonRoot = %v, want false (container-level override of the pod default)", sc.RunAsNonRoot)
	}

	// ... but with the smallest possible privilege surface.
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be true")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp profile should be RuntimeDefault")
	}
	if sc.Capabilities == nil {
		t.Fatal("capabilities is nil")
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", sc.Capabilities.Drop)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "CHOWN" {
		t.Errorf("capabilities.add = %v, want [CHOWN] and nothing else", sc.Capabilities.Add)
	}
}

func TestBuildStatefulSet_DataOwnerInitContainer_UsesConfiguredUIDAndGID(t *testing.T) {
	instance := newTestInstance("data-owner-uid")
	instance.Spec.Security.PodSecurityContext = &openclawv1alpha1.PodSecurityContextSpec{
		RunAsUser:  Ptr(int64(2000)),
		RunAsGroup: Ptr(int64(3000)),
	}

	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	c := findDataOwnerInitContainer(t, sts.Spec.Template.Spec.InitContainers)
	if c == nil {
		t.Fatal("init-data-owner container not found")
	}
	if !strings.Contains(c.Command[2], `want="2000:3000"`) {
		t.Errorf("script should chown to the configured runAsUser:runAsGroup, got:\n%s", c.Command[2])
	}
}

func TestBuildStatefulSet_DataOwnerInitContainer_DisabledViaSpec(t *testing.T) {
	instance := newTestInstance("data-owner-off")
	instance.Spec.Storage.FixOwnership = Ptr(false)

	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	initContainers := sts.Spec.Template.Spec.InitContainers

	if c := findDataOwnerInitContainer(t, initContainers); c != nil {
		t.Fatal("init-data-owner should not be present when storage.fixOwnership=false")
	}
	if len(initContainers) == 0 || initContainers[0].Name != "init-config" {
		t.Errorf("with the ownership fix disabled, init-config should be first again; got %v", initContainers)
	}
}

func TestBuildStatefulSet_DataOwnerInitContainer_RegistryOverride(t *testing.T) {
	instance := newTestInstance("data-owner-registry")
	instance.Spec.Registry = "mirror.example.com"

	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	c := findDataOwnerInitContainer(t, sts.Spec.Template.Spec.InitContainers)
	if c == nil {
		t.Fatal("init-data-owner container not found")
	}
	if !strings.HasPrefix(c.Image, "mirror.example.com/") {
		t.Errorf("image = %q, want registry override prefix", c.Image)
	}
}

func TestIsDataOwnershipFixEnabled(t *testing.T) {
	instance := newTestInstance("flag")
	if !IsDataOwnershipFixEnabled(instance) {
		t.Error("nil should default to enabled")
	}
	instance.Spec.Storage.FixOwnership = Ptr(true)
	if !IsDataOwnershipFixEnabled(instance) {
		t.Error("true should be enabled")
	}
	instance.Spec.Storage.FixOwnership = Ptr(false)
	if IsDataOwnershipFixEnabled(instance) {
		t.Error("false should be disabled")
	}
}
