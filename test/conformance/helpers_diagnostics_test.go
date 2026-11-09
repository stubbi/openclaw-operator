package conformance

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

func TestReadinessDiagnostics(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := openclawv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "owned"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{
			{Name: "TOKEN", Value: "do-not-log-config"},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "test-registry: manifest unknown",
			}},
		}}},
	}
	foreign := pod.DeepCopy()
	foreign.Name = "do-not-log-foreign"
	foreign.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&openclawv1alpha1.OpenClawInstance{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "owned"}},
		pod, foreign,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "owned"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
	).Build()
	diagnostics := readinessDiagnostics(context.Background(), c, "owned", "target")
	for _, want := range []string{"instance conditions:", "pods in test namespace: 1", "pod blocked phase=Pending", "ImagePullBackOff", "manifest unknown", "PVC storage phase=Pending"} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("diagnostics missing %q: %s", want, diagnostics)
		}
	}
	for _, forbidden := range []string{"do-not-log-config", "do-not-log-foreign"} {
		if strings.Contains(diagnostics, forbidden) {
			t.Errorf("diagnostics exposed %q: %s", forbidden, diagnostics)
		}
	}
	missing := readinessDiagnostics(context.Background(), c, "empty", "missing")
	for _, want := range []string{"instance status unavailable:", "pods in test namespace: 0", "PVCs in test namespace: 0"} {
		if !strings.Contains(missing, want) {
			t.Errorf("empty-namespace diagnostics missing %q: %s", want, missing)
		}
	}
}
