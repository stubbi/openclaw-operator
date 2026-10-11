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

package e2e

import (
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// diagnosticLogTailLines bounds how much of each container log is written to
// the spec output when a pod fails to become ready.
const diagnosticLogTailLines = "150"

// dumpPodDiagnostics writes the state a human needs to explain why a pod never
// became ready: per-container status (including the last termination), the
// namespace events, and the tail of the current and previous logs of every
// container that is not ready.
//
// It deliberately avoids `kubectl describe pod` and never prints the pod spec:
// specs in this suite carry API keys as plain env values.
//
// Call it before the test namespace is deleted; everything it reads goes away
// with the namespace.
func dumpPodDiagnostics(namespace, podName string) {
	GinkgoWriter.Printf("\n===== diagnostics for pod %s/%s =====\n", namespace, podName)

	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, pod); err != nil {
		GinkgoWriter.Printf("could not get pod: %v\n", err)
		return
	}
	GinkgoWriter.Printf("phase=%s reason=%q message=%q\n", pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		GinkgoWriter.Printf("condition %s=%s reason=%q message=%q\n", c.Type, c.Status, c.Reason, c.Message)
	}

	var notReady []string
	report := func(kind string, statuses []corev1.ContainerStatus) {
		for i := range statuses {
			cs := &statuses[i]
			GinkgoWriter.Printf("%s %s: ready=%t restarts=%d", kind, cs.Name, cs.Ready, cs.RestartCount)
			switch {
			case cs.State.Waiting != nil:
				GinkgoWriter.Printf(" waiting reason=%q message=%q", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			case cs.State.Running != nil:
				GinkgoWriter.Printf(" running since=%s", cs.State.Running.StartedAt.UTC().Format("15:04:05"))
			case cs.State.Terminated != nil:
				GinkgoWriter.Printf(" terminated exit=%d reason=%q", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
			}
			if t := cs.LastTerminationState.Terminated; t != nil {
				GinkgoWriter.Printf(" | last termination exit=%d reason=%q signal=%d", t.ExitCode, t.Reason, t.Signal)
			}
			GinkgoWriter.Println()
			if !cs.Ready && cs.State.Terminated == nil {
				notReady = append(notReady, cs.Name)
			}
		}
	}
	report("init container", pod.Status.InitContainerStatuses)
	report("container", pod.Status.ContainerStatuses)

	kubectl := func(title string, args ...string) {
		out, err := exec.Command("kubectl", args...).CombinedOutput()
		GinkgoWriter.Printf("----- %s -----\n%s\n", title, strings.TrimSpace(string(out)))
		if err != nil {
			GinkgoWriter.Printf("(kubectl %s: %v)\n", strings.Join(args, " "), err)
		}
	}

	kubectl("events", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp",
		"-o", "custom-columns=TIME:.lastTimestamp,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message")
	for _, name := range notReady {
		kubectl("logs "+name, "logs", podName, "-n", namespace, "-c", name, "--tail="+diagnosticLogTailLines)
		kubectl("previous logs "+name, "logs", podName, "-n", namespace, "-c", name, "--previous", "--tail="+diagnosticLogTailLines)
	}
	GinkgoWriter.Printf("===== end diagnostics for pod %s/%s =====\n\n", namespace, podName)
}
