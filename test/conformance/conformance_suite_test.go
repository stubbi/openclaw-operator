package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Conformance suite: categories live in sibling files:
//   - negative_test.go             webhook + CEL deny paths
//   - idempotency_test.go          re-reconcile no-op canary
//   - gitops_coexistence_test.go   SSA does not flap with external writers
//   - upgrade_test.go              prior-release -> HEAD chart upgrade
//   - failure_modes_test.go        operator restart mid-reconcile recovery
//
// All scenarios run against a live kind cluster with the operator installed.
// Ordinary local runs skip when KUBECONFIG is unset. Explicit full runs require
// a readable configuration file before any cluster setup or client creation.
func TestConformance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "openclaw-operator conformance suite")
}

var (
	suiteCtx    context.Context
	suiteCancel context.CancelFunc
)

var _ = BeforeSuite(func() {
	enabled, err := conformanceConfiguration(os.Getenv("KUBECONFIG"), os.Getenv("CONFORMANCE_FULL") == "1")
	Expect(err).NotTo(HaveOccurred())
	if !enabled {
		Skip("KUBECONFIG not set: conformance suite requires a live kind cluster with the operator installed")
	}
	suiteCtx, suiteCancel = context.WithCancel(context.Background())
	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)
})

// conformanceConfiguration checks opt-in without parsing a config or contacting
// an API server. Local runs retain their existing optional-config behavior.
func conformanceConfiguration(kubeconfig string, full bool) (bool, error) {
	const requirement = "CONFORMANCE_FULL=1 requires an explicit readable, non-empty KUBECONFIG file"
	if kubeconfig == "" {
		if full {
			return false, errors.New(requirement)
		}
		return false, nil
	}
	if !full {
		return true, nil
	}
	file, err := os.Open(kubeconfig)
	if err != nil {
		return false, fmt.Errorf("%s: %w", requirement, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("%s: %w", requirement, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false, errors.New(requirement)
	}
	return true, nil
}

var _ = AfterSuite(func() {
	if suiteCancel != nil {
		suiteCancel()
	}
})
