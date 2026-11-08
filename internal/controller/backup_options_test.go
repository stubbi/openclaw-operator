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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestPeriodicBackupCredentialsRemainSingleArguments(t *testing.T) {
	for _, envAuth := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "workload identity"}[envAuth], func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "argv")
			stub := "#!/bin/sh\nprintf '%s\\0' \"$@\" __CALL_END__ >> \"$ARGS_FILE\"\nif [ \"$1\" = lsf ]; then printf '2000-01-01/\\n'; fi\n"
			if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			// The container test covers Alpine date; this test isolates argv
			// handling and also runs on hosts with BSD date.
			clock := "#!/bin/sh\ncase \"$*\" in *+%s*) echo 1800000000 ;; *) echo 2026-11-08 ;; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "date"), []byte(clock), 0o755); err != nil {
				t.Fatal(err)
			}
			creds := &s3Credentials{Bucket: "test", Provider: "Other", Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", EnvAuth: envAuth}
			instance := &openclawv1alpha1.OpenClawInstance{ObjectMeta: metav1.ObjectMeta{Name: "literal", Namespace: "default"}}
			cron := buildBackupCronJob(instance, creds, "synthetic-secret")
			script := strings.ReplaceAll(cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command[2], "$$", "$")
			cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
			secret := "test secret'$VALUE"
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "ARGS_FILE="+logPath,
				"S3_ENDPOINT="+creds.Endpoint, "S3_REGION="+creds.Region,
				"S3_ACCESS_KEY_ID=test-key", "S3_SECRET_ACCESS_KEY="+secret)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("periodic script: %v: %s", err, out)
			}
			out, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			var args []string
			for _, arg := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
				if arg != "__CALL_END__" {
					args = append(args, arg)
					continue
				}
				calls++
				want := []string{"--s3-provider=Other", "--s3-endpoint=" + creds.Endpoint, "--s3-region=" + creds.Region}
				if envAuth {
					want = append(want, "--s3-env-auth=true")
				} else {
					want = append(want, "--s3-access-key-id=test-key", "--s3-secret-access-key="+secret)
				}
				for _, flag := range want {
					found := false
					for _, got := range args {
						if got == flag {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s lost one literal flag %q: %q", args[0], flag, args)
					}
				}
				if envAuth {
					for _, flag := range args {
						if strings.HasPrefix(flag, "--s3-access-key-id=") || strings.HasPrefix(flag, "--s3-secret-access-key=") {
							t.Fatalf("static credential leaked into workload identity args: %q", args)
						}
					}
				}
				args = nil
			}
			if calls != 4 {
				t.Fatalf("expected sync/copy/list/purge, got %d calls", calls)
			}
		})
	}
}
