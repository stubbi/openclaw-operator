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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The init-uv script must not fail the pod start when ~/.local/bin/uv is a
// symlink that does not resolve inside the init container (#626). These tests
// run the real script with /data and /usr/local/bin/uv rewritten to temporary
// directories.

type uvScriptEnv struct {
	data   string
	srcUv  string
	script string
}

func newUvScriptEnv(t *testing.T) *uvScriptEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(root, "image-bin")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcUv := filepath.Join(srcDir, "uv")
	if err := os.WriteFile(srcUv, []byte("#!/bin/sh\necho \"uv 0.0.0-image\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(uvInitScript, "/usr/local/bin/uv", srcUv)
	// Rewrite the init container's /data mount point but not the gateway
	// paths (/home/openclaw/...) that the script maps back onto it.
	script = strings.ReplaceAll(script, "/data/", data+"/")
	script = strings.ReplaceAll(script, "\"/data\"", "\""+data+"\"")
	if strings.Contains(script, " /data/") || strings.Contains(script, "=/data/") || strings.Contains(script, "\"/data/") {
		t.Fatalf("script still references /data after rewrite:\n%s", script)
	}
	return &uvScriptEnv{data: data, srcUv: srcUv, script: script}
}

func (e *uvScriptEnv) run(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "sh", "-c", e.script).CombinedOutput()
	if err != nil {
		t.Fatalf("init-uv script failed: %v\n%s", err, out)
	}
	return string(out)
}

func (e *uvScriptEnv) uvPath() string {
	return filepath.Join(e.data, ".local", "bin", "uv")
}

func (e *uvScriptEnv) mkBin(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.uvPath()), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertImageUvCopied(t *testing.T, e *uvScriptEnv) {
	t.Helper()
	fi, err := os.Lstat(e.uvPath())
	if err != nil {
		t.Fatalf("uv not installed: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("uv is still a symlink, want a regular file")
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("uv is not executable: %v", fi.Mode())
	}
	got, err := os.ReadFile(e.uvPath())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(e.srcUv)
	if !bytes.Equal(got, want) {
		t.Fatalf("uv content = %q, want the image's uv", got)
	}
	if _, err := os.Lstat(e.uvPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestUvInitScript_FreshVolumeInstallsUv(t *testing.T) {
	e := newUvScriptEnv(t)
	out := e.run(t)
	assertImageUvCopied(t, e)
	if !strings.Contains(out, "installed") {
		t.Errorf("output = %q, want install message", out)
	}
	for _, d := range []string{".local/bin", ".cache", ".config", "skills"} {
		if _, err := os.Stat(filepath.Join(e.data, d)); err != nil {
			t.Errorf("expected %s to be created: %v", d, err)
		}
	}
}

func TestUvInitScript_ExistingBinaryIsKept(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	if err := os.WriteFile(e.uvPath(), []byte("#!/bin/sh\necho user\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := e.run(t)
	got, _ := os.ReadFile(e.uvPath())
	if string(got) != "#!/bin/sh\necho user\n" {
		t.Errorf("existing uv was overwritten: %q", got)
	}
	if !strings.Contains(out, "already installed") {
		t.Errorf("output = %q, want already installed", out)
	}
}

// The case from #626: an absolute link created from the gateway whose target
// was removed. It must be replaced, not written through, and must not fail.
func TestUvInitScript_DanglingAbsoluteSymlinkIsReplaced(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	if err := os.Symlink("/home/openclaw/.openclaw/tools/uv/gone/uv", e.uvPath()); err != nil {
		t.Fatal(err)
	}
	out := e.run(t)
	assertImageUvCopied(t, e)
	if !strings.Contains(out, "replacing dangling uv symlink") {
		t.Errorf("output = %q, want replacement message", out)
	}
}

func TestUvInitScript_DanglingRelativeSymlinkIsReplaced(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	if err := os.Symlink("uv-missing", e.uvPath()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	assertImageUvCopied(t, e)
}

// A link that is valid for the gateway (target under ~/.openclaw, which is
// the data volume root) dangles at its literal path here. The script maps it
// back to /data and must keep it, so a user's pinned uv survives restarts.
func TestUvInitScript_GatewayStateDirSymlinkIsKept(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	pinned := filepath.Join(e.data, "tools", "uv", "x", "uv")
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\necho pinned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := "/home/openclaw/.openclaw/tools/uv/x/uv"
	if err := os.Symlink(link, e.uvPath()); err != nil {
		t.Fatal(err)
	}
	out := e.run(t)
	got, err := os.Readlink(e.uvPath())
	if err != nil || got != link {
		t.Fatalf("symlink changed: %q, %v", got, err)
	}
	if b, _ := os.ReadFile(pinned); string(b) != "#!/bin/sh\necho pinned\n" {
		t.Errorf("pinned uv was written through: %q", b)
	}
	if !strings.Contains(out, "already installed") {
		t.Errorf("output = %q, want already installed", out)
	}
}

func TestUvInitScript_GatewayLocalSymlinkIsKept(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	pinned := filepath.Join(e.data, ".local", "share", "uv-pinned")
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned, []byte("pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := "/home/openclaw/.local/share/uv-pinned"
	if err := os.Symlink(link, e.uvPath()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if got, _ := os.Readlink(e.uvPath()); got != link {
		t.Fatalf("symlink changed to %q", got)
	}
}

// A stale temporary file, even a dangling link, from an interrupted earlier
// run must not block the copy.
func TestUvInitScript_StaleTempFileIsIgnored(t *testing.T) {
	e := newUvScriptEnv(t)
	e.mkBin(t)
	if err := os.Symlink("/nonexistent/uv", e.uvPath()+".tmp"); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	assertImageUvCopied(t, e)
}

func TestUvInitScript_Idempotent(t *testing.T) {
	e := newUvScriptEnv(t)
	e.run(t)
	out := e.run(t)
	assertImageUvCopied(t, e)
	if !strings.Contains(out, "already installed") {
		t.Errorf("second run output = %q, want already installed", out)
	}
}

func TestBuildUvInitContainer_UsesUvInitScript(t *testing.T) {
	c := buildUvInitContainer(newTestInstance("uv-script"))
	if len(c.Command) != 3 || c.Command[2] != uvInitScript {
		t.Fatalf("init-uv command = %v, want sh -c uvInitScript", c.Command)
	}
}
