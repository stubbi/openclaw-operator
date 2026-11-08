package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConformanceConfigurationGuard(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		kubeconfig string
		full       bool
		enabled    bool
		wantError  bool
	}{
		{name: "local absent config"},
		{name: "local explicit config", kubeconfig: config, enabled: true},
		{name: "full absent config", full: true, wantError: true},
		{name: "full missing file", kubeconfig: filepath.Join(dir, "missing"), full: true, wantError: true},
		{name: "full empty file", kubeconfig: empty, full: true, wantError: true},
		{name: "full directory", kubeconfig: dir, full: true, wantError: true},
		{name: "full owned config", kubeconfig: config, full: true, enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled, err := conformanceConfiguration(tc.kubeconfig, tc.full)
			if enabled != tc.enabled || (err != nil) != tc.wantError {
				t.Fatalf("enabled=%v, err=%v; want enabled=%v, error=%v", enabled, err, tc.enabled, tc.wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "CONFORMANCE_FULL=1 requires") {
				t.Fatalf("unexpected configuration error: %v", err)
			}
		})
	}
}
