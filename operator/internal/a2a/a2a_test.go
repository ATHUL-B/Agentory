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

package a2a

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

const (
	testNamespace = "team-a"
	testAgent     = "oncall"
	gatewayURL    = "http://agentory-gateway.agentgateway-system.svc.cluster.local:8080/agents/team-a/oncall"
)

func newAgent(mutate ...func(*agentoryv1alpha1.Agent)) *agentoryv1alpha1.Agent {
	a := &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testAgent, Namespace: testNamespace, UID: "uid-1"},
		Spec: agentoryv1alpha1.AgentSpec{
			Description: "Payments on-call",
			Runtime:     agentoryv1alpha1.RuntimeContainer,
			Lifecycle:   agentoryv1alpha1.LifecycleService,
			Model:       agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
		},
	}
	for _, m := range mutate {
		m(a)
	}
	return a
}

// requiredCardFields are required by the A2A AgentCard schema.
var requiredCardFields = []string{"protocolVersion", "name", "description", "url", "version", "capabilities",
	"defaultInputModes", "defaultOutputModes", "skills"}

const condTrue = "True"

func TestCardDescribesAgentAtGatewayURL(t *testing.T) {
	card := CardBuilder{Config: agentruntime.DefaultGatewayConfig()}.Build(newAgent(), "oncall-abc")
	if card.URL != gatewayURL || card.Name != testAgent || card.Version != "oncall-abc" {
		t.Errorf("card = %+v", card)
	}
	if card.ProtocolVersion != ProtocolVersion || card.PreferredTransport != "JSONRPC" || card.Description != "Payments on-call" {
		t.Errorf("card = %+v", card)
	}
	data, _ := json.Marshal(card)
	var generic map[string]any
	_ = json.Unmarshal(data, &generic)
	for _, field := range requiredCardFields {
		if _, ok := generic[field]; !ok {
			t.Errorf("card JSON lacks required field %q", field)
		}
	}
	if string(data) == "" || generic["skills"] == nil {
		t.Error("skills must be an empty list, not null")
	}
}

func TestCardListsDeclarativeSkillsAndDefaultsDescription(t *testing.T) {
	agent := newAgent(func(a *agentoryv1alpha1.Agent) {
		a.Spec.Description = ""
		a.Spec.Declarative = &agentoryv1alpha1.DeclarativeSpec{Skills: []agentoryv1alpha1.SkillRef{{Name: "k8s-triage"}}}
	})
	card := CardBuilder{Config: agentruntime.DefaultGatewayConfig()}.Build(agent, "r")
	if len(card.Skills) != 1 || card.Skills[0].ID != "k8s-triage" {
		t.Errorf("skills = %+v", card.Skills)
	}
	if card.Description != "Agentory agent team-a/oncall" {
		t.Errorf("description = %q", card.Description)
	}
}

func TestExposed(t *testing.T) {
	cases := map[string]struct {
		a2a  *agentoryv1alpha1.A2ASpec
		want bool
	}{
		"no a2a block": {nil, true},
		"expose unset": {&agentoryv1alpha1.A2ASpec{}, true},
		"expose true":  {&agentoryv1alpha1.A2ASpec{Expose: new(true)}, true},
		"expose false": {&agentoryv1alpha1.A2ASpec{Expose: new(false)}, false},
	}
	for name, tc := range cases {
		if got := Exposed(newAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.A2A = tc.a2a })); got != tc.want {
			t.Errorf("%s: Exposed = %v, want %v", name, got, tc.want)
		}
	}
}

func newManager(t *testing.T, objs ...client.Object) (*RouteManager, client.Client) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewRouteManager(c, scheme, agentruntime.DefaultGatewayConfig()), c
}

func getUnstructured(t *testing.T, c client.Client, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testAgent}, u); err != nil {
		t.Fatalf("get %s: %v", gvk.Kind, err)
	}
	return u
}

func TestEnsureCardWritesConfigMap(t *testing.T) {
	agent := newAgent()
	m, c := newManager(t, agent)
	if err := m.EnsureCard(context.Background(), agent, "oncall-r1"); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "oncall-agent-card"}, cm); err != nil {
		t.Fatal(err)
	}
	var card Card
	if err := json.Unmarshal([]byte(cm.Data[agentruntime.AgentCardFile]), &card); err != nil || card.Version != "oncall-r1" {
		t.Errorf("card in ConfigMap = %+v, %v", card, err)
	}
	if !metav1.IsControlledBy(cm, agent) {
		t.Error("ConfigMap must be owned by the agent")
	}
}

func TestEnsureRouteCreatesBackendAndRoute(t *testing.T) {
	agent := newAgent()
	m, c := newManager(t, agent)
	st, err := m.EnsureRoute(context.Background(), agent, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ready || st.Reason != ReasonRoutePending {
		t.Errorf("fresh route status = %+v, want pending", st)
	}

	backend := getUnstructured(t, c, BackendGVK)
	host, _, _ := unstructured.NestedString(backend.Object, "spec", "a2a", "host")
	port, _, _ := unstructured.NestedInt64(backend.Object, "spec", "a2a", "port")
	if host != "oncall.team-a.svc.cluster.local" || port != 9000 {
		t.Errorf("backend a2a = %s:%d", host, port)
	}

	route := getUnstructured(t, c, HTTPRouteGVK)
	if !metav1.IsControlledBy(route, agent) || !metav1.IsControlledBy(backend, agent) {
		t.Error("route and backend must be owned by the agent")
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	parent := parents[0].(map[string]any)
	if parent["name"] != "agentory-gateway" || parent["namespace"] != "agentgateway-system" {
		t.Errorf("parentRef = %v", parent)
	}
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	rule := rules[0].(map[string]any)
	prefix, _, _ := unstructured.NestedString(rule["matches"].([]any)[0].(map[string]any), "path", "value")
	if prefix != "/agents/team-a/oncall" {
		t.Errorf("path prefix = %q", prefix)
	}
	ref := rule["backendRefs"].([]any)[0].(map[string]any)
	if ref["kind"] != "AgentgatewayBackend" || ref["name"] != testAgent {
		t.Errorf("backendRef = %v", ref)
	}
}

func TestRouteStatusFollowsGatewayConditions(t *testing.T) {
	m, _ := newManager(t)
	cond := func(conditionType, status, message string) map[string]any {
		return map[string]any{"type": conditionType, "status": status, "message": message}
	}
	parent := func(gateway string, conds ...any) map[string]any {
		return map[string]any{"parentRef": map[string]any{"name": gateway}, "conditions": conds}
	}
	route := func(accepted, resolved string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"parents": []any{
			parent("other-gateway", cond("Accepted", condTrue, "")),
			parent("agentory-gateway", cond("Accepted", accepted, "no listener"), cond("ResolvedRefs", resolved, "backend missing")),
		}}}}
		u.SetName(testAgent)
		return u
	}
	cases := map[string]struct {
		accepted, resolved, reason string
		ready                      bool
	}{
		"accepted":       {condTrue, condTrue, ReasonRouteAccepted, true},
		"rejected":       {"False", condTrue, ReasonRouteRejected, false},
		"unresolved ref": {condTrue, "False", ReasonRouteRejected, false},
		"not yet":        {"", "", ReasonRoutePending, false},
	}
	for name, tc := range cases {
		st := m.routeStatus(route(tc.accepted, tc.resolved))
		if st.Ready != tc.ready || st.Reason != tc.reason {
			t.Errorf("%s: status = %+v", name, st)
		}
	}
}

func TestEnsureRouteRefusesForeignRoute(t *testing.T) {
	agent := newAgent()
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(HTTPRouteGVK)
	foreign.SetNamespace(testNamespace)
	foreign.SetName(testAgent)
	m, c := newManager(t, agent, foreign)

	st, err := m.EnsureRoute(context.Background(), agent, 8080)
	if err != nil || st.Reason != ReasonResourceConflict {
		t.Fatalf("EnsureRoute = %+v, %v; want ResourceConflict", st, err)
	}
	if len(getUnstructured(t, c, HTTPRouteGVK).GetOwnerReferences()) != 0 {
		t.Error("foreign route was modified")
	}
}

func TestRemoveDeletesOnlyOwnedObjects(t *testing.T) {
	agent := newAgent()
	m, c := newManager(t, agent)
	ctx := context.Background()
	if err := m.EnsureCard(ctx, agent, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureRoute(ctx, agent, 8080); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{HTTPRouteGVK, BackendGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testAgent}, u); err == nil {
			t.Errorf("%s still exists after Remove", gvk.Kind)
		}
	}
	if err := m.Remove(ctx, agent); err != nil {
		t.Errorf("second Remove = %v, want nil", err)
	}
}
