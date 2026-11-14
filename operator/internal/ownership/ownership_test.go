/*
Copyright 2026.

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

package ownership

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "ns"

func setup(t *testing.T, objs ...client.Object) (client.Client, *k8sruntime.Scheme, *corev1.ConfigMap) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// Any object type works as an owner; a ConfigMap keeps the test free of CRDs.
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: testNamespace, UID: "uid-owner"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, owner)...).Build()
	return c, scheme, owner
}

func TestEnsureCreatesOwnedObjectWithLabels(t *testing.T) {
	c, scheme, owner := setup(t)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNamespace}}
	err := Ensure(context.Background(), c, scheme, owner, sa, map[string]string{"team": "x"}, func() {
		sa.AutomountServiceAccountToken = new(false)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.ServiceAccount{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(sa), got)
	if !metav1.IsControlledBy(got, owner) || got.Labels["team"] != "x" || *got.AutomountServiceAccountToken {
		t.Errorf("got %+v", got)
	}
}

func TestEnsureRefusesForeignObject(t *testing.T) {
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNamespace,
		Labels: map[string]string{"owner": "someone"}}}
	c, scheme, owner := setup(t, foreign)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNamespace}}
	err := Ensure(context.Background(), c, scheme, owner, sa, map[string]string{"team": "x"}, func() {})
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	got := &corev1.ServiceAccount{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(sa), got)
	if len(got.OwnerReferences) != 0 || got.Labels["team"] != "" {
		t.Errorf("foreign object was modified: %+v", got.ObjectMeta)
	}
}

func TestDeleteOnlyRemovesOwnedObjects(t *testing.T) {
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: testNamespace}}
	c, scheme, owner := setup(t, foreign)
	ctx := context.Background()
	mine := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: testNamespace}}
	if err := Ensure(ctx, c, scheme, owner, mine, nil, func() {}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"mine", "foreign", "missing"} {
		if err := Delete(ctx, c, owner, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}); err != nil {
			t.Errorf("Delete(%s) = %v", name, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(mine), &corev1.ServiceAccount{}); err == nil {
		t.Error("owned object not deleted")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.ServiceAccount{}); err != nil {
		t.Error("foreign object was deleted")
	}
}

func TestMergeLabels(t *testing.T) {
	got := MergeLabels(map[string]string{"a": "1", "b": "1"}, map[string]string{"b": "2", "c": "2"})
	if len(got) != 3 || got["a"] != "1" || got["b"] != "2" || got["c"] != "2" {
		t.Errorf("MergeLabels = %v", got)
	}
}
