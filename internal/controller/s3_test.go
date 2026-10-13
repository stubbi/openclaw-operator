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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
	"github.com/paperclipinc/openclaw-operator/internal/resources"
)

var _ = Describe("S3 Helpers", func() {
	Context("pvcNameForInstance", func() {
		It("Should return the existing claim name when specified", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-instance",
					Namespace: "default",
				},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{
					Storage: openclawv1alpha1.StorageSpec{
						Persistence: openclawv1alpha1.PersistenceSpec{
							ExistingClaim: "my-existing-pvc",
						},
					},
				},
			}
			Expect(pvcNameForInstance(instance)).To(Equal("my-existing-pvc"))
		})

		It("Should return default PVC name when no existing claim is specified", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-instance",
					Namespace: "default",
				},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{},
			}
			Expect(pvcNameForInstance(instance)).To(Equal(resources.PVCName(instance)))
		})
	})

	Context("getTenantID", func() {
		It("Should return the tenant label value when present", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "oc-tenant-cus_123",
					Labels: map[string]string{
						LabelTenant: "cus_456",
					},
				},
			}
			Expect(getTenantID(instance)).To(Equal("cus_456"))
		})

		It("Should extract tenant from namespace when label is missing", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "oc-tenant-cus_789",
				},
			}
			Expect(getTenantID(instance)).To(Equal("cus_789"))
		})

		It("Should return namespace as-is when not in oc-tenant format", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
			}
			Expect(getTenantID(instance)).To(Equal("default"))
		})
	})

	Context("buildRcloneJob", func() {
		var creds *s3Credentials

		BeforeEach(func() {
			creds = &s3Credentials{
				Bucket:   "test-bucket",
				KeyID:    "key123",
				AppKey:   "secret456",
				Endpoint: "https://s3.us-west-000.backblazeb2.com",
				Provider: "Other",
			}
		})

		It("Should build a backup Job with correct args and SecurityContext", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "backup")
			job := buildRcloneJob("myinst-backup", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, creds, true, nil, nil, "", "myinst-s3-credentials", &openclawv1alpha1.BackupSpec{})

			Expect(job.Name).To(Equal("myinst-backup"))
			Expect(job.Namespace).To(Equal("oc-tenant-t1"))
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))
			Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(86400)))

			// Verify container
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(RcloneImage))
			Expect(container.Args[0]).To(Equal("sync"))
			Expect(container.Args[1]).To(Equal("/data/")) // PVC source for backup

			// Verify SecurityContext
			podSC := job.Spec.Template.Spec.SecurityContext
			Expect(*podSC.RunAsUser).To(Equal(int64(1000)))
			Expect(*podSC.RunAsGroup).To(Equal(int64(1000)))
			Expect(*podSC.FSGroup).To(Equal(int64(1000)))

			// Verify PVC volume
			vol := job.Spec.Template.Spec.Volumes[0]
			Expect(vol.PersistentVolumeClaim.ClaimName).To(Equal("myinst-data"))

			// Verify env vars
			var envNames []string
			for _, e := range container.Env {
				envNames = append(envNames, e.Name)
			}
			Expect(envNames).To(ContainElements("S3_ENDPOINT", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"))
			Expect(envNames).NotTo(ContainElement("S3_REGION"))

			// Verify credentials use secretKeyRef (not plaintext Value)
			for _, e := range container.Env {
				if e.Name == "S3_ACCESS_KEY_ID" || e.Name == "S3_SECRET_ACCESS_KEY" {
					Expect(e.Value).To(BeEmpty(), "credential env var %s should not have plaintext Value", e.Name)
					Expect(e.ValueFrom).NotTo(BeNil(), "credential env var %s should use ValueFrom", e.Name)
					Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
					Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("myinst-s3-credentials"))
					Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal(e.Name))
				}
			}

			// Verify --skip-links flag is present (skips symlinks to avoid broken-link errors)
			Expect(container.Args).To(ContainElement("--skip-links"))

			// Verify non-sensitive env vars use plain Value
			for _, e := range container.Env {
				if e.Name == "S3_ENDPOINT" {
					Expect(e.Value).To(Equal("https://s3.us-west-000.backblazeb2.com"))
					Expect(e.ValueFrom).To(BeNil())
				}
			}

			// Verify no --s3-region flag when Region is empty
			for _, arg := range container.Args {
				Expect(arg).NotTo(HavePrefix("--s3-region"))
			}

			// Verify no ServiceAccountName when not set
			Expect(job.Spec.Template.Spec.ServiceAccountName).To(BeEmpty())
		})

		It("Should build a restore Job with S3 as source", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "restore")
			job := buildRcloneJob("myinst-restore", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, creds, false, nil, nil, "", "myinst-s3-credentials", &openclawv1alpha1.BackupSpec{})

			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Args[0]).To(Equal("sync"))
			// For restore, dest is /data/
			Expect(container.Args[2]).To(Equal("/data/"))
			// --skip-links is present for restore too (harmless - no symlinks on S3 side)
			Expect(container.Args).To(ContainElement("--skip-links"))

			vol := job.Spec.Template.Spec.Volumes[0]
			Expect(vol.PersistentVolumeClaim.ClaimName).To(Equal("myinst-data"))
		})

		It("Should propagate nodeSelector and tolerations to Job pod", func() {
			nodeSelector := map[string]string{"openclaw.rocks/nodepool": "openclaw"}
			tolerations := []corev1.Toleration{
				{
					Key:      "openclaw.rocks/dedicated",
					Operator: corev1.TolerationOpEqual,
					Value:    "openclaw",
					Effect:   corev1.TaintEffectNoSchedule,
				},
			}
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "backup")
			job := buildRcloneJob("myinst-backup", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, creds, true, nodeSelector, tolerations, "", "myinst-s3-credentials", &openclawv1alpha1.BackupSpec{})

			Expect(job.Spec.Template.Spec.NodeSelector).To(Equal(nodeSelector))
			Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Tolerations[0].Key).To(Equal("openclaw.rocks/dedicated"))
			Expect(job.Spec.Template.Spec.Tolerations[0].Value).To(Equal("openclaw"))
		})

		It("Should include --s3-region flag and S3_REGION env var when Region is set", func() {
			creds.Region = "eu-west-1"
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "backup")
			job := buildRcloneJob("myinst-backup", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, creds, true, nil, nil, "", "myinst-s3-credentials", &openclawv1alpha1.BackupSpec{})

			container := job.Spec.Template.Spec.Containers[0]

			// Verify --s3-region flag is present
			Expect(container.Args).To(ContainElement("--s3-region=$(S3_REGION)"))

			// Verify S3_REGION env var is present as plain Value (not sensitive)
			var regionEnv *corev1.EnvVar
			for i, e := range container.Env {
				if e.Name == "S3_REGION" {
					regionEnv = &container.Env[i]
					break
				}
			}
			Expect(regionEnv).NotTo(BeNil())
			Expect(regionEnv.Value).To(Equal("eu-west-1"))
			Expect(regionEnv.ValueFrom).To(BeNil())
		})

		It("Should use --s3-env-auth=true and omit static credential args/env when EnvAuth is true", func() {
			envAuthCreds := &s3Credentials{
				Bucket:   "test-bucket",
				Endpoint: "https://s3.us-east-1.amazonaws.com",
				Provider: "AWS",
				EnvAuth:  true,
			}
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "backup")
			job := buildRcloneJob("myinst-backup", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, envAuthCreds, true, nil, nil, "", "", &openclawv1alpha1.BackupSpec{})

			container := job.Spec.Template.Spec.Containers[0]

			// Verify --s3-env-auth=true is present
			Expect(container.Args).To(ContainElement("--s3-env-auth=true"))

			// Verify --s3-provider uses the configured provider
			Expect(container.Args).To(ContainElement("--s3-provider=AWS"))

			// Verify static credential flags are NOT present
			for _, arg := range container.Args {
				Expect(arg).NotTo(HavePrefix("--s3-access-key-id"))
				Expect(arg).NotTo(HavePrefix("--s3-secret-access-key"))
			}

			// Verify only S3_ENDPOINT env var is set (no static cred env vars)
			var envNames []string
			for _, e := range container.Env {
				envNames = append(envNames, e.Name)
			}
			Expect(envNames).To(ContainElement("S3_ENDPOINT"))
			Expect(envNames).NotTo(ContainElement("S3_ACCESS_KEY_ID"))
			Expect(envNames).NotTo(ContainElement("S3_SECRET_ACCESS_KEY"))
		})

		It("Should set ServiceAccountName on the Job pod when provided", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
				},
			}
			labels := backupLabels(instance, "backup")
			job := buildRcloneJob("myinst-backup", "oc-tenant-t1", "myinst-data", "backups/t1/myinst/2026-01-01T000000Z", labels, creds, true, nil, nil, "my-irsa-sa", "myinst-s3-credentials", &openclawv1alpha1.BackupSpec{})

			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("my-irsa-sa"))
		})
	})

	Context("isJobFinished", func() {
		It("Should return false for an active Job", func() {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
			}
			finished, _ := isJobFinished(job)
			Expect(finished).To(BeFalse())
		})

		It("Should return true with Complete for a succeeded Job", func() {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}
			finished, condType := isJobFinished(job)
			Expect(finished).To(BeTrue())
			Expect(condType).To(Equal(batchv1.JobComplete))
		})

		It("Should return true with Failed for a failed Job", func() {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}
			finished, condType := isJobFinished(job)
			Expect(finished).To(BeTrue())
			Expect(condType).To(Equal(batchv1.JobFailed))
		})
	})

	Context("backupLabels", func() {
		It("Should include tenant, instance, and job-type labels", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-cus_123",
					Labels: map[string]string{
						LabelTenant: "cus_123",
					},
				},
			}
			labels := backupLabels(instance, "backup")
			Expect(labels[LabelTenant]).To(Equal("cus_123"))
			Expect(labels[LabelInstance]).To(Equal("myinst"))
			Expect(labels["openclaw.rocks/job-type"]).To(Equal("backup"))
			Expect(labels[LabelManagedBy]).To(Equal("openclaw-operator"))
		})
	})

	Context("mirrorSecretName", func() {
		It("Should return instance name with -s3-credentials suffix", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
			}
			Expect(mirrorSecretName(instance)).To(Equal("my-agent-s3-credentials"))
		})
	})

	Context("backupCronJobName", func() {
		It("Should return instance name with -backup-periodic suffix", func() {
			instance := &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
			}
			Expect(backupCronJobName(instance)).To(Equal("my-agent-backup-periodic"))
		})
	})

	Context("buildBackupCronJob", func() {
		var creds *s3Credentials
		var instance *openclawv1alpha1.OpenClawInstance

		BeforeEach(func() {
			creds = &s3Credentials{
				Bucket:   "test-bucket",
				KeyID:    "key123",
				AppKey:   "secret456",
				Endpoint: "https://s3.us-west-000.backblazeb2.com",
				Provider: "Other",
			}
			instance = &openclawv1alpha1.OpenClawInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myinst",
					Namespace: "oc-tenant-t1",
					Labels: map[string]string{
						LabelTenant: "cus_123",
					},
				},
				Spec: openclawv1alpha1.OpenClawInstanceSpec{
					Backup: openclawv1alpha1.BackupSpec{
						Schedule: "0 2 * * *",
					},
				},
			}
		})

		It("Should set the cron schedule from spec", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(cronJob.Spec.Schedule).To(Equal("0 2 * * *"))
		})

		It("Should set ConcurrencyPolicy to Forbid", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		})

		It("Should use default history limits when not specified", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(*cronJob.Spec.SuccessfulJobsHistoryLimit).To(Equal(int32(3)))
			Expect(*cronJob.Spec.FailedJobsHistoryLimit).To(Equal(int32(1)))
		})

		It("Should use custom history limits when specified", func() {
			historyLimit := int32(5)
			failedLimit := int32(2)
			instance.Spec.Backup.HistoryLimit = &historyLimit
			instance.Spec.Backup.FailedHistoryLimit = &failedLimit

			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(*cronJob.Spec.SuccessfulJobsHistoryLimit).To(Equal(int32(5)))
			Expect(*cronJob.Spec.FailedJobsHistoryLimit).To(Equal(int32(2)))
		})

		It("Should mount PVC read-write so fsGroup can apply ownership", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
			Expect(container.VolumeMounts).To(HaveLen(1))
			Expect(container.VolumeMounts[0].ReadOnly).To(BeFalse())
			Expect(container.VolumeMounts[0].MountPath).To(Equal("/data"))

			vol := cronJob.Spec.JobTemplate.Spec.Template.Spec.Volumes[0]
			Expect(vol.PersistentVolumeClaim.ReadOnly).To(BeFalse())
		})

		It("Should set pod affinity for co-location with StatefulSet pod", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			affinity := cronJob.Spec.JobTemplate.Spec.Template.Spec.Affinity
			Expect(affinity).NotTo(BeNil())
			Expect(affinity.PodAffinity).NotTo(BeNil())
			Expect(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))

			term := affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
			Expect(term.TopologyKey).To(Equal("kubernetes.io/hostname"))
			Expect(term.LabelSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "openclaw"))
			Expect(term.LabelSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "myinst"))
		})

		It("Should use incremental sync with daily snapshots and retention cleanup", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
			Expect(container.Command).To(HaveLen(3))
			Expect(container.Command[0]).To(Equal("sh"))
			Expect(container.Command[1]).To(Equal("-c"))
			cmd := container.Command[2]
			// Step 1: incremental sync to fixed "latest" path (with --skip-links to skip symlinks)
			Expect(cmd).To(ContainSubstring("rclone sync /data/"))
			Expect(cmd).To(ContainSubstring("--skip-links"))
			Expect(cmd).To(ContainSubstring("/latest"))
			Expect(cmd).To(ContainSubstring(":s3:test-bucket/backups/cus_123/myinst/periodic"))
			// Step 2: daily snapshot
			Expect(cmd).To(ContainSubstring("rclone copy"))
			Expect(cmd).To(ContainSubstring("/snapshots/${TODAY}"))
			// Step 3: retention cleanup
			Expect(cmd).To(ContainSubstring("rclone purge"))
			Expect(cmd).To(ContainSubstring("CUTOFF"))
			// Should NOT use the old timestamped full-copy approach
			Expect(cmd).NotTo(ContainSubstring("periodic/${TIMESTAMP}"))
		})

		It("Should set security context with UID/GID 1000", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			podSC := cronJob.Spec.JobTemplate.Spec.Template.Spec.SecurityContext
			Expect(*podSC.RunAsUser).To(Equal(int64(1000)))
			Expect(*podSC.RunAsGroup).To(Equal(int64(1000)))
			Expect(*podSC.FSGroup).To(Equal(int64(1000)))
		})

		It("Should use rclone image and reference credentials via secretKeyRef", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(RcloneImage))
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))

			var envNames []string
			for _, e := range container.Env {
				envNames = append(envNames, e.Name)
			}
			Expect(envNames).To(ContainElements("S3_ENDPOINT", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"))

			// Verify credentials use secretKeyRef (not plaintext Value)
			for _, e := range container.Env {
				if e.Name == "S3_ACCESS_KEY_ID" || e.Name == "S3_SECRET_ACCESS_KEY" {
					Expect(e.Value).To(BeEmpty(), "credential env var %s should not have plaintext Value", e.Name)
					Expect(e.ValueFrom).NotTo(BeNil(), "credential env var %s should use ValueFrom", e.Name)
					Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
					Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("myinst-s3-credentials"))
					Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal(e.Name))
				}
			}
		})

		It("Should set periodic-backup label", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(cronJob.Labels["openclaw.rocks/job-type"]).To(Equal("periodic-backup"))
		})

		It("Should propagate nodeSelector and tolerations from spec.availability", func() {
			instance.Spec.Availability.NodeSelector = map[string]string{
				"openclaw.rocks/nodepool": "openclaw",
			}
			instance.Spec.Availability.Tolerations = []corev1.Toleration{
				{
					Key:      "openclaw.rocks/dedicated",
					Operator: corev1.TolerationOpEqual,
					Value:    "openclaw",
					Effect:   corev1.TaintEffectNoSchedule,
				},
			}
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(podSpec.NodeSelector).To(Equal(map[string]string{
				"openclaw.rocks/nodepool": "openclaw",
			}))
			Expect(podSpec.Tolerations).To(HaveLen(1))
			Expect(podSpec.Tolerations[0].Key).To(Equal("openclaw.rocks/dedicated"))
			Expect(podSpec.Tolerations[0].Value).To(Equal("openclaw"))
			Expect(podSpec.Tolerations[0].Effect).To(Equal(corev1.TaintEffectNoSchedule))
		})

		It("Should leave nodeSelector and tolerations nil when not set", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(podSpec.NodeSelector).To(BeNil())
			Expect(podSpec.Tolerations).To(BeNil())
		})

		It("Should set explicit Kubernetes default fields", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			spec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
			Expect(spec.DNSPolicy).To(Equal(corev1.DNSClusterFirst))
			Expect(spec.SchedulerName).To(Equal("default-scheduler"))
			Expect(spec.TerminationGracePeriodSeconds).NotTo(BeNil())

			container := spec.Containers[0]
			Expect(container.TerminationMessagePath).To(Equal("/dev/termination-log"))
			Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageReadFile))
		})

		It("Should set activeDeadlineSeconds on the Job to kill stuck backups", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).NotTo(BeNil())
			Expect(*cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(int64(3600)))
		})

		It("Should set startingDeadlineSeconds on the CronJob to skip stale runs", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			Expect(cronJob.Spec.StartingDeadlineSeconds).NotTo(BeNil())
			Expect(*cronJob.Spec.StartingDeadlineSeconds).To(Equal(int64(600)))
		})

		It("Should use --s3-env-auth=true in rclone command and omit static cred env vars when EnvAuth is true", func() {
			envAuthCreds := &s3Credentials{
				Bucket:   "test-bucket",
				Endpoint: "https://s3.us-east-1.amazonaws.com",
				Provider: "AWS",
				EnvAuth:  true,
			}
			cronJob := buildBackupCronJob(instance, envAuthCreds, "")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

			// Verify rclone command uses --s3-env-auth=true and --s3-provider=AWS
			Expect(container.Command[2]).To(ContainSubstring("--s3-env-auth=true"))
			Expect(container.Command[2]).To(ContainSubstring("--s3-provider=AWS"))
			Expect(container.Command[2]).NotTo(ContainSubstring("--s3-access-key-id"))
			Expect(container.Command[2]).NotTo(ContainSubstring("--s3-secret-access-key"))

			// Verify only S3_ENDPOINT env var is set
			var envNames []string
			for _, e := range container.Env {
				envNames = append(envNames, e.Name)
			}
			Expect(envNames).To(ContainElement("S3_ENDPOINT"))
			Expect(envNames).NotTo(ContainElement("S3_ACCESS_KEY_ID"))
			Expect(envNames).NotTo(ContainElement("S3_SECRET_ACCESS_KEY"))
		})

		It("Should set ServiceAccountName on CronJob pod when spec.backup.serviceAccountName is set", func() {
			instance.Spec.Backup.ServiceAccountName = "my-irsa-sa"
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(podSpec.ServiceAccountName).To(Equal("my-irsa-sa"))
		})

		It("Should leave ServiceAccountName empty when spec.backup.serviceAccountName is not set", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(podSpec.ServiceAccountName).To(BeEmpty())
		})

		It("Should include --s3-region flag in rclone command and S3_REGION env var when Region is set", func() {
			creds.Region = "us-west-2"
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

			// Verify --s3-region flag is present in the shell command
			Expect(container.Command[2]).To(ContainSubstring(`--s3-region="${S3_REGION}"`))

			// Verify S3_REGION env var is present as plain Value (not sensitive)
			var regionEnv *corev1.EnvVar
			for i, e := range container.Env {
				if e.Name == "S3_REGION" {
					regionEnv = &container.Env[i]
					break
				}
			}
			Expect(regionEnv).NotTo(BeNil())
			Expect(regionEnv.Value).To(Equal("us-west-2"))
			Expect(regionEnv.ValueFrom).To(BeNil())
		})

		It("Should not include --s3-region flag or S3_REGION env var when Region is empty", func() {
			cronJob := buildBackupCronJob(instance, creds, "myinst-s3-credentials")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

			// Verify --s3-region flag is NOT present in the shell command
			Expect(container.Command[2]).NotTo(ContainSubstring("--s3-region"))

			// Verify S3_REGION env var is NOT present
			var envNames []string
			for _, e := range container.Env {
				envNames = append(envNames, e.Name)
			}
			Expect(envNames).NotTo(ContainElement("S3_REGION"))
		})

		It("Should include --s3-region with env-auth mode when Region is set", func() {
			envAuthCreds := &s3Credentials{
				Bucket:   "test-bucket",
				Endpoint: "https://s3.us-west-2.amazonaws.com",
				Provider: "AWS",
				Region:   "us-west-2",
				EnvAuth:  true,
			}
			cronJob := buildBackupCronJob(instance, envAuthCreds, "")
			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

			// Verify --s3-region flag is present in the shell command
			Expect(container.Command[2]).To(ContainSubstring(`--s3-region="${S3_REGION}"`))

			// Verify S3_REGION env var is present
			var regionEnv *corev1.EnvVar
			for i, e := range container.Env {
				if e.Name == "S3_REGION" {
					regionEnv = &container.Env[i]
					break
				}
			}
			Expect(regionEnv).NotTo(BeNil())
			Expect(regionEnv.Value).To(Equal("us-west-2"))
		})
	})
})
