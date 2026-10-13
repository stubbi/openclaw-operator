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

package controller

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

func TestBackupOptionsAreLiteralAndDoNotFilterRestore(t *testing.T) {
	spec := openclawv1alpha1.BackupSpec{
		Excludes:  []string{"/npm/**", "/cache with spaces/**", "/quote'/**", "/$(S3_SECRET_ACCESS_KEY)/**", "/`; exit 99; #/**"},
		Transfers: resources.Ptr(int32(32)), Checkers: resources.Ptr(int32(64)), FastList: true,
	}
	creds := &s3Credentials{Bucket: "test", Provider: "Other", Endpoint: "http://127.0.0.1:1", EnvAuth: true}
	for _, isBackup := range []bool{true, false} {
		job := buildRcloneJob("test", "default", "data", "snapshot", nil, creds, isBackup, nil, nil, "", "", &spec)
		args := job.Spec.Template.Spec.Containers[0].Args
		got := strings.Join(args, "\n")
		if isBackup {
			for _, pattern := range spec.Excludes {
				if !strings.Contains(got, "--exclude="+strings.ReplaceAll(pattern, "$", "$$")) {
					t.Errorf("lost literal filter %q", pattern)
				}
			}
		} else if strings.Contains(got, "--exclude") || strings.Contains(got, "--transfers=32") {
			t.Fatalf("backup options leaked into restore: %v", args)
		}
	}
	// Model kubelet's escaped-dollar handling, then let a real shell parse the
	// generated quoting. Shell commands or environment refs must remain data.
	script := "set -- " + strings.ReplaceAll(shellBackupRcloneArgs(&spec), "$$", "$") + "; printf '%s\\0' \"$@\""
	out, err := exec.CommandContext(context.Background(), "sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	want := []string{"--skip-links", "--transfers=32", "--checkers=64", "-v", "--fast-list"}
	for _, pattern := range spec.Excludes {
		want = append(want, "--exclude="+pattern)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shell changed filter arguments:\n got %q\nwant %q", got, want)
	}
}
