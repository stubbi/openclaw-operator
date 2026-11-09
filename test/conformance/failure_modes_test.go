package conformance

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Failure modes: the operator process is killed mid-flight (pod deleted while
// an instance is being reconciled). controller-runtime restarts the manager
// and the reconcile loop must converge the instance back to Ready without
// manual intervention and without destructively recreating owned objects.
//
// The operator is assumed to be installed in the openclaw-system namespace by
// `make conformance-install` (Helm release name openclaw-operator).
const (
	operatorNamespace = "openclaw-system"
	operatorSelector  = "app.kubernetes.io/name=openclaw-operator"
)

var _ = Describe("failure modes", Ordered, func() {
	var (
		ns       string
		c        = newClient
		instName = "failure-recover"
	)

	manifest := `apiVersion: openclaw.rocks/v1alpha1
kind: OpenClawInstance
metadata:
  name: ` + instName + `
spec:
  image:
    repository: ghcr.io/openclaw/openclaw
    tag: latest
  resources:
    limits:
      cpu: "2"
      memory: 2Gi
  storage:
    persistence:
      enabled: true
      size: 1Gi
`

	BeforeAll(func() {
		ns = freshNamespace("failure")
		DeferCleanup(func() {
			deleteNamespace(ns)
		})

		out, err := kubectlApply(addNamespace(manifest, ns))
		Expect(err).ToNot(HaveOccurred(), "applying instance: %s", out)
	})

	It("becomes Ready", func() {
		waitForInstanceReady(suiteCtx, c(), ns, instName, 3*time.Minute)
	})

	It("recovers after the operator pod is killed mid-reconcile", func() {
		cl := c()
		before := captureFingerprint(suiteCtx, cl, ns, instName)

		// Poke the instance so a reconcile is in flight, then kill the operator.
		forceRequeue(suiteCtx, cl, ns, instName)
		out, err := kubectl("delete", "pod", "-n", operatorNamespace, "-l", operatorSelector, "--wait=false")
		if err != nil && !strings.Contains(out, "NotFound") {
			Skip("operator pod not found in " + operatorNamespace + " (suite expects make conformance-install): " + out)
		}

		// The Deployment restarts the manager; the new leader must converge.
		waitForInstanceReady(suiteCtx, cl, ns, instName, 3*time.Minute)

		// Poke again post-recovery and confirm owned objects are not churned.
		forceRequeue(suiteCtx, cl, ns, instName)
		time.Sleep(15 * time.Second)
		after := captureFingerprint(suiteCtx, cl, ns, instName)
		expectFingerprintUnchanged(&before, &after)
	})
})
