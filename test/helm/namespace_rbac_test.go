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

package helm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise actual authorization with the rendered chart's bindings. Merely
// finding a namespaces rule in a Role misses that Namespace is cluster-scoped.
func TestNamespaceReadAuthorization(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required (installed in CI)")
	}
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if stopErr := env.Stop(); stopErr != nil {
			t.Error(stopErr)
		}
	})
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, name := range []string{"operator-system", "tenant-a", "tenant-b", "unwatched"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, scoped := range []bool{true, false} {
		name := "cluster"
		if scoped {
			name = "scoped"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"template", name, "../../charts/openclaw-operator", "--namespace", "operator-system"}
			if scoped {
				args = append(args, "--set", "watchNamespaces={tenant-a,tenant-b}")
			}
			rendered, err := exec.CommandContext(ctx, "helm", args...).Output()
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
			for {
				obj := &unstructured.Unstructured{}
				decodeErr := decoder.Decode(obj)
				if errors.Is(decodeErr, io.EOF) {
					break
				}
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				switch obj.GetKind() {
				case "Role", "RoleBinding", "ServiceAccount":
					if obj.GetNamespace() == "" {
						obj.SetNamespace("operator-system")
					}
				case "ClusterRole", "ClusterRoleBinding":
				default:
					continue
				}
				if createErr := admin.Create(ctx, obj); createErr != nil {
					t.Fatal(createErr)
				}
			}
			user, err := env.AddUser(envtest.User{Name: "system:serviceaccount:operator-system:" + name + "-openclaw-operator", Groups: []string{"system:authenticated"}}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			api, err := kubernetes.NewForConfig(user.Config())
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				verb, name string
				allowed    bool
			}{
				{"get", "tenant-a", true}, {"get", "tenant-b", true},
				{"get", "unwatched", !scoped}, {"get", "operator-system", !scoped},
				{"list", "", false}, {"watch", "", false}, {"patch", "tenant-a", false},
			} {
				review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: check.verb, Resource: "namespaces", Name: check.name}}}
				// RBAC authorizer informers observe newly-created bindings asynchronously.
				deadline := time.Now().Add(5 * time.Second)
				for {
					result, err := api.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if result.Status.Allowed == check.allowed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s namespace %q: allowed=%t, want %t (%s)", check.verb, check.name, result.Status.Allowed, check.allowed, result.Status.Reason)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		})
	}
}
