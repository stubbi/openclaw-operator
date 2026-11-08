package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubectlStdinPreservesDiagnostics(t *testing.T) {
	dir := t.TempDir()
	shim := "#!/bin/sh\ncat > \"$KUBECTL_CAPTURE_STDIN\"\nprintf '%s\\n' \"$*\" > \"$KUBECTL_CAPTURE_ARGS\"\nprintf '%s' \"$KUBECTL_STDOUT\"\nprintf '%s' \"$KUBECTL_STDERR\" >&2\nexit \"$KUBECTL_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("KUBECONFIG", filepath.Join(dir, "unused-config"))
	t.Setenv("KUBECTL_CAPTURE_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("KUBECTL_CAPTURE_ARGS", filepath.Join(dir, "args"))
	for _, tc := range []struct {
		name   string
		stderr string
		exit   string
	}{
		{name: "API rejection", stderr: "OpenClawInstance is invalid: spec.image: image tag must not be empty", exit: "1"},
		{name: "successful apply", exit: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBECTL_STDOUT", "stdout marker\n")
			t.Setenv("KUBECTL_STDERR", tc.stderr)
			t.Setenv("KUBECTL_EXIT", tc.exit)
			out, err := kubectlApply("manifest marker\n")
			if !strings.Contains(out, "stdout marker") || !strings.Contains(out, tc.stderr) {
				t.Fatalf("combined output lost diagnostics: %q", out)
			}
			if tc.exit == "0" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("lost original exit status: %v", err)
				}
				if !strings.Contains(err.Error(), tc.stderr) {
					t.Fatalf("error lost API rejection diagnostics: %v", err)
				}
			}
			for file, want := range map[string]string{"stdin": "manifest marker\n", "args": "apply -f -\n"} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil || string(data) != want {
					t.Fatalf("%s=%q, err=%v; want %q", file, data, err, want)
				}
			}
		})
	}
}
