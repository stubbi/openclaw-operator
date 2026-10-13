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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

// Opt in with OPENCLAW_BACKUP_CONTAINER_TEST=1. Uses only disposable container
// storage and a loopback S3 server; no host mounts, ports, or real credentials.
func TestBackupRcloneContainer(t *testing.T) {
	if os.Getenv("OPENCLAW_BACKUP_CONTAINER_TEST") != "1" {
		t.Skip("set OPENCLAW_BACKUP_CONTAINER_TEST=1 to run the rclone image integration test")
	}
	creds := &s3Credentials{Bucket: "test", Provider: "Other", Endpoint: "http://127.0.0.1:9000", KeyID: "test-key", AppKey: "test-secret"}
	instance := &openclawv1alpha1.OpenClawInstance{ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "default"}, Spec: openclawv1alpha1.OpenClawInstanceSpec{Backup: openclawv1alpha1.BackupSpec{
		Schedule: "0 2 * * *", Excludes: []string{"/cache/**", "/cache with spaces/**", "/quote'/**"}, Transfers: resources.Ptr(int32(32)), Checkers: resources.Ptr(int32(64)), FastList: true,
	}}}
	// Expand the generated exec-form Kubernetes args using dummy test env.
	replacer := strings.NewReplacer("$(S3_ENDPOINT)", creds.Endpoint, "$(S3_ACCESS_KEY_ID)", creds.KeyID, "$(S3_SECRET_ACCESS_KEY)", creds.AppKey, "$$", "$")
	command := func(backup bool, path string, spec *openclawv1alpha1.BackupSpec) string {
		job := buildRcloneJob("smoke", "default", "data", path, nil, creds, backup, nil, nil, "", "test-secret", spec)
		args := append([]string{"rclone"}, job.Spec.Template.Spec.Containers[0].Args...)
		for i, arg := range args {
			args[i] = "'" + strings.ReplaceAll(replacer.Replace(arg), "'", "'\"'\"'") + "'"
		}
		return strings.Join(args, " ")
	}
	cron := buildBackupCronJob(instance, creds, "test-secret")
	cronScript := replacer.Replace(cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command[2])
	script := `set -eu
mkdir -p /store/test /data/cache /data/workspace
printf important > /data/workspace/state.json
for i in $(seq 1 1000); do printf cache > /data/cache/$i; done
rclone serve s3 /store --addr 127.0.0.1:9000 --auth-key test-key,test-secret >/tmp/server.log 2>&1 &
server=$!
trap 'kill "$server"' EXIT
export S3_ENDPOINT=http://127.0.0.1:9000 S3_ACCESS_KEY_ID=test-key S3_SECRET_ACCESS_KEY=test-secret
for i in $(seq 1 50); do if rclone lsd :s3:test --s3-provider=Other --s3-endpoint="$S3_ENDPOINT" --s3-access-key-id="$S3_ACCESS_KEY_ID" --s3-secret-access-key="$S3_SECRET_ACCESS_KEY" --retries=1 --low-level-retries=1 >/dev/null 2>&1; then break; fi; sleep 0.1; done
`
	// Two fresh snapshot destinations both contain every file with defaults.
	script += command(true, "baseline-1", &openclawv1alpha1.BackupSpec{}) + "\n" + command(true, "baseline-2", &openclawv1alpha1.BackupSpec{}) + "\n"
	script += `test "$(find /store/test/baseline-1 -type f | wc -l)" -eq 1001
test "$(find /store/test/baseline-2 -type f | wc -l)" -eq 1001
`
	script += command(true, "filtered", &instance.Spec.Backup) + "\n"
	script += `test "$(find /store/test/filtered -type f | wc -l)" -eq 1
test "$(cat /store/test/filtered/workspace/state.json)" = important
`
	// Seed an old latest containing caches, then run the actual generated
	// periodic script: newly copied snapshots must honor the new excludes.
	script += command(true, "backups/default/smoke/periodic/latest", &openclawv1alpha1.BackupSpec{}) + "\nsh -c '" + strings.ReplaceAll(cronScript, "'", "'\"'\"'") + "'\n"
	script += `test "$(find /store/test/backups/default/smoke/periodic/snapshots -type f | wc -l)" -eq 1
`
	script += "rm -rf /data/*\n" + command(false, "baseline-1", &instance.Spec.Backup) + "\n"
	script += `test "$(find /data -type f | wc -l)" -eq 1001
echo 'PASS: fresh snapshots 1001/1001 files, filtered backup 1 file, periodic snapshot 1 file, unfiltered restore 1001 files'
`
	runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "docker", "run", "--rm", "-i", "--network", "none", "--entrypoint", "sh", RcloneImage, "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rclone integration: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	t.Log(lines[len(lines)-1])
}
