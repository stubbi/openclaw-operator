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

// OpenClaw >= 2026.9.6 refuses to create a temporary workspace below a
// directory that is group/world writable without the sticky bit. The root of
// an emptyDir is exactly that (mode 2777) once fsGroup is applied, so the main
// container must get its /tmp from a subdirectory that init-tmp-dir creates
// with mode 1777.

func findInitContainer(t *testing.T, containers []corev1.Container, name string) (int, *corev1.Container) {
	t.Helper()
	for i := range containers {
		if containers[i].Name == name {
			return i, &containers[i]
		}
	}
	t.Fatalf("init container %q not found", name)
	return -1, nil
}

func TestBuildStatefulSet_MainTmpIsMountedThroughStickySubPath(t *testing.T) {
	instance := newTestInstance("sticky-tmp")
	sts := BuildStatefulSet(instance, "", nil, nil, nil)

	var main *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == "openclaw" {
			main = &sts.Spec.Template.Spec.Containers[i]
		}
	}
	if main == nil {
		t.Fatal("openclaw container not found")
	}

	var tmpMounts int
	for _, m := range main.VolumeMounts {
		if m.MountPath != "/tmp" {
			continue
		}
		tmpMounts++
		if m.Name != "tmp" {
			t.Errorf("/tmp volume = %q, want tmp", m.Name)
		}
		if m.SubPath != MainTmpSubPath {
			t.Errorf("/tmp subPath = %q, want %q: mounting the emptyDir root exposes its 2777 mode", m.SubPath, MainTmpSubPath)
		}
	}
	if tmpMounts != 1 {
		t.Fatalf("main container has %d mounts at /tmp, want 1", tmpMounts)
	}

	tmpVol := findVolume(sts.Spec.Template.Spec.Volumes, "tmp")
	if tmpVol == nil || tmpVol.EmptyDir == nil {
		t.Fatal("tmp volume should be an emptyDir")
	}
}

func TestBuildStatefulSet_TmpDirInitContainer(t *testing.T) {
	instance := newTestInstance("sticky-tmp-init")
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	_, c := findInitContainer(t, sts.Spec.Template.Spec.InitContainers, TmpDirInitContainerName)

	if len(c.Command) != 3 {
		t.Fatalf("command = %v, want sh -c <script>", c.Command)
	}
	script := c.Command[2]
	for _, want := range []string{
		"mkdir -p /tmp-volume/" + MainTmpSubPath,
		"chmod 1777 /tmp-volume/" + MainTmpSubPath,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q does not contain %q", script, want)
		}
	}

	// The subdirectory must be created inside the volume the main container
	// mounts, seen from its root.
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != "tmp" || c.VolumeMounts[0].SubPath != "" {
		t.Errorf("volume mounts = %+v, want the tmp volume root only", c.VolumeMounts)
	}
	if c.VolumeMounts[0].MountPath != "/tmp-volume" {
		t.Errorf("mount path = %q, want /tmp-volume", c.VolumeMounts[0].MountPath)
	}

	// It has to own the directory it creates, so it must run as the pod UID
	// (no runAsUser override), and it must stay admissible under the
	// restricted Pod Security Standard.
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("security context is nil")
	}
	if sc.RunAsUser != nil {
		t.Errorf("runAsUser = %d, want unset so the pod UID owns the directory", *sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot should be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Errorf("capabilities = %+v, want drop ALL and no additions", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp profile should be RuntimeDefault")
	}

	// Kubernetes defaults must be set explicitly to avoid reconcile loops.
	if c.TerminationMessagePath != corev1.TerminationMessagePathDefault || c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Error("termination message defaults should be set explicitly")
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("imagePullPolicy = %q, want IfNotPresent", c.ImagePullPolicy)
	}
}

func TestBuildStatefulSet_TmpDirInitContainer_SecurityContext(t *testing.T) {
	for _, tc := range []struct {
		name             string
		podUID           *int64
		podNonRoot       *bool
		containerUID     *int64
		containerNonRoot *bool
		wantUID          int64
		wantNonRoot      bool
	}{
		{name: "defaults", wantUID: 1000, wantNonRoot: true},
		{name: "pod UID", podUID: Ptr(int64(1500)), wantUID: 1500, wantNonRoot: true},
		{name: "container UID", podUID: Ptr(int64(1500)), containerUID: Ptr(int64(2000)), wantUID: 2000, wantNonRoot: true},
		{name: "explicit root", containerUID: Ptr(int64(0)), containerNonRoot: Ptr(false), wantUID: 0, wantNonRoot: false},
		{name: "root UID alone preserves non-root requirement", containerUID: Ptr(int64(0)), wantUID: 0, wantNonRoot: true},
		{name: "inherit pod non-root setting", podNonRoot: Ptr(false), wantUID: 1000, wantNonRoot: false},
		{name: "container non-root overrides pod", podNonRoot: Ptr(false), containerUID: Ptr(int64(2000)), containerNonRoot: Ptr(true), wantUID: 2000, wantNonRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := newTestInstance("sticky-tmp-security")
			instance.Spec.Security.PodSecurityContext = &openclawv1alpha1.PodSecurityContextSpec{
				RunAsUser: tc.podUID, RunAsNonRoot: tc.podNonRoot,
			}
			instance.Spec.Security.ContainerSecurityContext = &openclawv1alpha1.ContainerSecurityContextSpec{
				RunAsUser: tc.containerUID, RunAsNonRoot: tc.containerNonRoot,
			}
			sts := BuildStatefulSet(instance, "", nil, nil, nil)
			_, c := findInitContainer(t, sts.Spec.Template.Spec.InitContainers, TmpDirInitContainerName)
			sc := c.SecurityContext
			if sc == nil {
				t.Fatal("security context is nil")
			}
			uid := sc.RunAsUser
			if uid == nil {
				uid = sts.Spec.Template.Spec.SecurityContext.RunAsUser
			}
			if uid == nil {
				t.Fatal("effective runAsUser is nil")
			}
			if *uid != tc.wantUID {
				t.Errorf("effective runAsUser = %d, want %d", *uid, tc.wantUID)
			}
			if sc.RunAsNonRoot == nil {
				t.Fatal("runAsNonRoot is nil")
			}
			if *sc.RunAsNonRoot != tc.wantNonRoot {
				t.Errorf("runAsNonRoot = %t, want %t", *sc.RunAsNonRoot, tc.wantNonRoot)
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Error("allowPrivilegeEscalation should be false")
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Error("readOnlyRootFilesystem should be true")
			}
			if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
				t.Errorf("capabilities = %+v, want drop ALL and no additions", sc.Capabilities)
			}
			if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Error("seccomp profile should be RuntimeDefault")
			}
		})
	}
}

func TestBuildStatefulSet_TmpDirInitContainer_AlwaysPresent(t *testing.T) {
	// The main container cannot start without the subdirectory, so the init
	// container must not depend on any optional feature, including the root
	// ownership fix that restricted namespaces switch off.
	instance := newTestInstance("sticky-tmp-minimal")
	instance.Spec.Storage.FixOwnership = Ptr(false)
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	findInitContainer(t, sts.Spec.Template.Spec.InitContainers, TmpDirInitContainerName)
}

func TestBuildStatefulSet_TmpDirInitContainer_BeforeCustomInitContainers(t *testing.T) {
	instance := newTestInstance("sticky-tmp-order")
	instance.Spec.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox"}}
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	initContainers := sts.Spec.Template.Spec.InitContainers

	tmpIdx, _ := findInitContainer(t, initContainers, TmpDirInitContainerName)
	userIdx, _ := findInitContainer(t, initContainers, "user-init")
	if tmpIdx > userIdx {
		t.Errorf("init-tmp-dir (index %d) should run before custom init containers (index %d)", tmpIdx, userIdx)
	}
}

func TestBuildStatefulSet_TmpDirInitContainer_RegistryOverride(t *testing.T) {
	instance := newTestInstance("sticky-tmp-registry")
	instance.Spec.Registry = "registry.example.com"
	sts := BuildStatefulSet(instance, "", nil, nil, nil)
	_, c := findInitContainer(t, sts.Spec.Template.Spec.InitContainers, TmpDirInitContainerName)
	if !strings.HasPrefix(c.Image, "registry.example.com/") {
		t.Errorf("image = %q, want the registry override applied", c.Image)
	}
}
