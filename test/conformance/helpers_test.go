package conformance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

func run(cmd string, args ...string) (string, error) {
	c := exec.Command(cmd, args...)
	b, err := c.CombinedOutput()
	return string(b), err
}

// kubectlStdin runs kubectl with the given args, feeding stdin (used to pipe
// manifests via `-f -`).
func kubectlStdin(args []string, stdin string) (string, error) {
	c := exec.Command("kubectl", args...)
	c.Stdin = strings.NewReader(stdin)
	b, err := c.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return string(b), err
}

func kubectl(args ...string) (string, error) { return run("kubectl", args...) }

func kubectlApply(yaml string) (string, error) {
	return kubectlStdin([]string{"apply", "-f", "-"}, yaml)
}

func kubectlDelete(yaml string) (string, error) {
	return kubectlStdin([]string{"delete", "--ignore-not-found", "-f", "-"}, yaml)
}

func clientcmdPath() string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.kube/config"
}

func newClient() client.Client {
	cfg, err := clientcmd.BuildConfigFromFlags("", clientcmdPath())
	Expect(err).ToNot(HaveOccurred())
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(openclawv1alpha1.AddToScheme(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).ToNot(HaveOccurred())
	return c
}

func waitForInstanceReady(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst := &openclawv1alpha1.OpenClawInstance{}
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, inst)
		if err == nil && hasReadyTrue(inst) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	Fail(fmt.Sprintf("OpenClawInstance %s/%s did not become Ready within %s\n%s", ns, name, timeout,
		readinessDiagnostics(ctx, c, ns, name)))
}

// Capture status before the existing cleanup removes the failed test namespace.
// Do not log resource specs, configuration, or Secret data.
func readinessDiagnostics(ctx context.Context, c client.Client, ns, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lines []string
	inst := &openclawv1alpha1.OpenClawInstance{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, inst); err != nil {
		lines = append(lines, fmt.Sprintf("instance status unavailable: %v", err))
	} else {
		lines = append(lines, fmt.Sprintf("instance conditions: %+v", inst.Status.Conditions))
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ns)); err != nil {
		lines = append(lines, fmt.Sprintf("pod status unavailable: %v", err))
	} else {
		lines = append(lines, fmt.Sprintf("pods in test namespace: %d", len(pods.Items)))
		for _, pod := range pods.Items {
			lines = append(lines, fmt.Sprintf("pod %s phase=%s conditions=%+v", pod.Name, pod.Status.Phase, pod.Status.Conditions))
			statuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
			for _, status := range statuses {
				lines = append(lines, fmt.Sprintf("container %s state=%+v", status.Name, status.State))
			}
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, claims, client.InNamespace(ns)); err != nil {
		lines = append(lines, fmt.Sprintf("PVC status unavailable: %v", err))
	} else {
		lines = append(lines, fmt.Sprintf("PVCs in test namespace: %d", len(claims.Items)))
		for _, claim := range claims.Items {
			lines = append(lines, fmt.Sprintf("PVC %s phase=%s conditions=%+v", claim.Name, claim.Status.Phase, claim.Status.Conditions))
		}
	}
	return strings.Join(lines, "\n")
}

func hasReadyTrue(inst *openclawv1alpha1.OpenClawInstance) bool {
	for _, cond := range inst.Status.Conditions {
		if cond.Type == openclawv1alpha1.ConditionTypeReady && cond.Status == "True" {
			return true
		}
	}
	return false
}

// forceRequeue mutates a harmless annotation to force the controller to
// reconcile the instance again. A correct (idempotent) reconciler must not
// rewrite owned objects in response.
func forceRequeue(ctx context.Context, c client.Client, ns, name string) {
	inst := &openclawv1alpha1.OpenClawInstance{}
	Expect(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, inst)).To(Succeed())
	if inst.Annotations == nil {
		inst.Annotations = map[string]string{}
	}
	inst.Annotations["openclaw.rocks/conformance-poke"] = fmt.Sprintf("%d", time.Now().UnixNano())
	Expect(c.Update(ctx, inst)).To(Succeed())
}

type metaTuple struct {
	Generation      int64
	ResourceVersion string
}

// resourceFingerprint captures the generation + resourceVersion of the core
// owned objects so re-reconciles can be asserted as no-ops. Names follow the
// internal/resources naming helpers: StatefulSet/Service = instance.Name,
// ConfigMap = name-config, workspace ConfigMap = name-workspace, PVC = name-data.
type resourceFingerprint struct {
	StatefulSet        metaTuple
	Service            metaTuple
	ConfigMap          metaTuple
	WorkspaceConfigMap metaTuple
	PVC                metaTuple
}

func captureFingerprint(ctx context.Context, c client.Client, ns, name string) resourceFingerprint {
	fp := resourceFingerprint{}
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sts); err == nil {
		fp.StatefulSet = metaTuple{sts.Generation, sts.ResourceVersion}
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, svc); err == nil {
		fp.Service = metaTuple{svc.Generation, svc.ResourceVersion}
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-config"}, cm); err == nil {
		fp.ConfigMap = metaTuple{cm.Generation, cm.ResourceVersion}
	}
	wcm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-workspace"}, wcm); err == nil {
		fp.WorkspaceConfigMap = metaTuple{wcm.Generation, wcm.ResourceVersion}
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-data"}, pvc); err == nil {
		fp.PVC = metaTuple{pvc.Generation, pvc.ResourceVersion}
	}
	return fp
}

func expectFingerprintUnchanged(before, after *resourceFingerprint) {
	check := func(fieldName string, b, a metaTuple) {
		Expect(a.Generation).To(Equal(b.Generation),
			fmt.Sprintf("%s.metadata.generation changed: %d -> %d (idempotency broken)", fieldName, b.Generation, a.Generation))
		Expect(a.ResourceVersion).To(Equal(b.ResourceVersion),
			fmt.Sprintf("%s.metadata.resourceVersion changed: %s -> %s (idempotency broken)", fieldName, b.ResourceVersion, a.ResourceVersion))
	}
	check("StatefulSet", before.StatefulSet, after.StatefulSet)
	check("Service", before.Service, after.Service)
	check("ConfigMap", before.ConfigMap, after.ConfigMap)
	check("WorkspaceConfigMap", before.WorkspaceConfigMap, after.WorkspaceConfigMap)
	check("PVC", before.PVC, after.PVC)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	Expect(err).ToNot(HaveOccurred(), "reading %s", path)
	return string(b)
}

func freshNamespace(prefix string) string {
	ns := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	out, err := kubectl("create", "namespace", ns)
	Expect(err).ToNot(HaveOccurred(), "create ns: %s", out)
	return ns
}

func deleteNamespace(ns string) {
	_, _ = kubectl("delete", "namespace", ns, "--ignore-not-found", "--wait=false")
}

// addNamespace injects a namespace into every resource that lacks one. Simple
// string injection for test fixtures with a single metadata block.
func addNamespace(yaml, ns string) string {
	return strings.ReplaceAll(yaml, "\nmetadata:\n  name:", "\nmetadata:\n  namespace: "+ns+"\n  name:")
}
