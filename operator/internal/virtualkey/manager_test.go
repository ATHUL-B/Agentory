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

package virtualkey

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/llmgateway"
	gatewayfake "github.com/ATHUL-B/Agentory/operator/internal/llmgateway/fake"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

const (
	testNamespace = "team-a"
	testAgent     = "oncall"
	secretName    = testAgent + "-llm"
)

func newAgent() *agentoryv1alpha1.Agent {
	return &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testAgent, Namespace: testNamespace, UID: "uid-1"},
		Spec: agentoryv1alpha1.AgentSpec{
			Runtime:   agentoryv1alpha1.RuntimeContainer,
			Lifecycle: agentoryv1alpha1.LifecycleService,
			Model:     agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
		},
	}
}

func setup(t *testing.T, objs ...client.Object) (*Manager, client.Client, *gatewayfake.Gateway) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	gw := gatewayfake.New()
	return &Manager{Client: c, Scheme: scheme, Gateway: gw}, c, gw
}

func getSecret(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: secretName}, s); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	return s
}

func TestEnsureCreatesKeyAndSecret(t *testing.T) {
	agent := newAgent()
	m, c, gw := setup(t, agent)

	st, err := m.Ensure(context.Background(), agent)
	if err != nil || !st.Ready || st.Reason != ReasonKeyReady || st.KeyID == "" {
		t.Fatalf("Ensure = %+v, %v", st, err)
	}
	spec, ok := gw.Spec(st.KeyID)
	if !ok || spec.Name != "agentory:team-a/oncall" || spec.Provider != "mock" || spec.Model != "mock-model" {
		t.Errorf("gateway spec = %+v", spec)
	}
	if spec.Metadata["agentory.uid"] != "uid-1" {
		t.Errorf("metadata = %v", spec.Metadata)
	}

	s := getSecret(t, c)
	if string(s.Data[agentruntime.VirtualKeySecretKey]) == "" || s.Annotations[AnnotationKeyID] != st.KeyID {
		t.Errorf("secret = %+v", s)
	}
	if !metav1.IsControlledBy(s, agent) {
		t.Error("secret must be owned by the agent")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	agent := newAgent()
	m, c, gw := setup(t, agent)
	ctx := context.Background()
	first, _ := m.Ensure(ctx, agent)
	rv := getSecret(t, c).ResourceVersion

	second, err := m.Ensure(ctx, agent)
	if err != nil || second.KeyID != first.KeyID {
		t.Fatalf("second Ensure = %+v, %v", second, err)
	}
	if gw.Calls["Create"] != 1 || gw.Calls["Update"] != 0 {
		t.Errorf("gateway calls = %v, want one Create and no Update", gw.Calls)
	}
	if getSecret(t, c).ResourceVersion != rv {
		t.Error("second Ensure rewrote the secret")
	}
}

func TestEnsureUpdatesKeyWhenModelChanges(t *testing.T) {
	agent := newAgent()
	m, c, gw := setup(t, agent)
	ctx := context.Background()
	first, _ := m.Ensure(ctx, agent)
	value := string(getSecret(t, c).Data[agentruntime.VirtualKeySecretKey])

	agent.Spec.Model.Name = "other-model"
	second, err := m.Ensure(ctx, agent)
	if err != nil || second.KeyID != first.KeyID {
		t.Fatalf("Ensure after model change = %+v, %v (key must be updated in place)", second, err)
	}
	if spec, _ := gw.Spec(first.KeyID); spec.Model != "other-model" || gw.Calls["Update"] != 1 {
		t.Errorf("gateway spec = %+v, calls = %v", spec, gw.Calls)
	}
	if string(getSecret(t, c).Data[agentruntime.VirtualKeySecretKey]) != value {
		t.Error("updating the key must not change its value")
	}
}

func TestEnsureRecreatesKeyTheGatewayLost(t *testing.T) {
	agent := newAgent()
	m, c, gw := setup(t, agent)
	ctx := context.Background()
	first, _ := m.Ensure(ctx, agent)
	gw.Forget(first.KeyID)

	second, err := m.Ensure(ctx, agent)
	if err != nil || !second.Ready || second.Reason != ReasonKeyRotated {
		t.Fatalf("Ensure = %+v, %v; want KeyRotated", second, err)
	}
	if second.KeyID == first.KeyID {
		t.Error("a re-created key must have a new ID so pods roll")
	}
	if getSecret(t, c).Annotations[AnnotationKeyID] != second.KeyID {
		t.Error("secret not updated with the new key")
	}
}

func TestEnsureReplacesOrphanedKey(t *testing.T) {
	agent := newAgent()
	m, _, gw := setup(t, agent)
	ctx := context.Background()
	// A key exists in the gateway but its Secret was lost.
	orphan, _ := gw.Create(ctx, SpecFor(agent))

	st, err := m.Ensure(ctx, agent)
	if err != nil || !st.Ready {
		t.Fatalf("Ensure = %+v, %v", st, err)
	}
	if st.KeyID == orphan.ID || gw.Len() != 1 {
		t.Errorf("orphan not replaced: key %s, orphan %s, keys %d", st.KeyID, orphan.ID, gw.Len())
	}
}

func TestEnsureRefusesForeignSecret(t *testing.T) {
	agent := newAgent()
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNamespace},
		Data: map[string][]byte{"virtual-key": []byte("someone-elses")}}
	m, c, gw := setup(t, agent, foreign)

	st, err := m.Ensure(context.Background(), agent)
	if err != nil || st.Ready || st.Reason != ReasonResourceConflict {
		t.Fatalf("Ensure = %+v, %v; want ResourceConflict", st, err)
	}
	if gw.Len() != 0 {
		t.Error("no key may be created while the Secret name is taken")
	}
	if string(getSecret(t, c).Data["virtual-key"]) != "someone-elses" {
		t.Error("foreign secret was modified")
	}
}

func TestEnsureReportsGatewayErrors(t *testing.T) {
	agent := newAgent()
	m, _, gw := setup(t, agent)
	gw.Err = errors.New("connection refused")

	st, err := m.Ensure(context.Background(), agent)
	if err == nil || st.Ready || st.Reason != ReasonGatewayError {
		t.Fatalf("Ensure = %+v, %v; want GatewayError and an error to retry", st, err)
	}
}

func TestCleanupDeletesKeyAndOrphans(t *testing.T) {
	agent := newAgent()
	m, _, gw := setup(t, agent)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup(ctx, agent); err != nil || gw.Len() != 0 {
		t.Fatalf("Cleanup = %v, keys left %d", err, gw.Len())
	}

	// Orphan only (no secret).
	_, _ = gw.Create(ctx, llmgateway.VirtualKeySpec{Name: KeyName(agent)})
	m2, _, _ := setup(t, agent)
	m2.Gateway = gw
	if err := m2.Cleanup(ctx, agent); err != nil || gw.Len() != 0 {
		t.Errorf("Cleanup of orphan = %v, keys left %d", err, gw.Len())
	}

	// Nothing to delete is fine.
	if err := m2.Cleanup(ctx, agent); err != nil {
		t.Errorf("Cleanup with no key = %v", err)
	}
}
