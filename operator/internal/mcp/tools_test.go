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

package mcp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/a2a"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

const (
	testNamespace = "team-payments"
	registryNS    = "agentory-mcp"
	oncallURL     = "http://oncall-mcp.mcp-demo.svc.cluster.local:8080/mcp"
	oncall        = "oncall"
	objectName    = "payments-oncall-mcp"
)

func agentWith(servers ...agentoryv1alpha1.MCPServerRef) *agentoryv1alpha1.Agent {
	return &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-oncall", Namespace: testNamespace, UID: "uid-1"},
		Spec: agentoryv1alpha1.AgentSpec{
			Runtime:   agentoryv1alpha1.RuntimeDeclarative,
			Lifecycle: agentoryv1alpha1.LifecycleService,
			Model:     agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
			Tools:     &agentoryv1alpha1.ToolsSpec{MCPServers: servers},
		},
	}
}

func server(ns, name, url string) *agentoryv1alpha1.MCPServer {
	return &agentoryv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agentoryv1alpha1.MCPServerSpec{URL: url, Transport: agentoryv1alpha1.MCPTransportStreamableHTTP},
	}
}

func newManager(t *testing.T, objs ...client.Object) (*Manager, client.Client) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewManager(c, scheme, agentruntime.DefaultGatewayConfig(), registryNS), c
}

func get(t *testing.T, c client.Client, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: objectName}, u); err != nil {
		t.Fatalf("get %s: %v", gvk.Kind, err)
	}
	return u
}

func TestParseURL(t *testing.T) {
	cases := map[string]Target{
		oncallURL: {Name: oncall, Host: "oncall-mcp.mcp-demo.svc.cluster.local", Port: 8080, Path: "/mcp",
			Transport: agentoryv1alpha1.MCPTransportStreamableHTTP},
		"https://mcp.datadoghq.com/api/mcp": {Name: oncall, Host: "mcp.datadoghq.com", Port: 443, Path: "/api/mcp", TLS: true,
			Transport: agentoryv1alpha1.MCPTransportStreamableHTTP},
		"http://tools.internal": {Name: oncall, Host: "tools.internal", Port: 80, Path: "/mcp",
			Transport: agentoryv1alpha1.MCPTransportStreamableHTTP},
	}
	for raw, want := range cases {
		got, err := ParseURL(oncall, raw, "")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	sse, _ := ParseURL(oncall, "http://legacy:9000", agentoryv1alpha1.MCPTransportSSE)
	if sse.Path != "/sse" || sse.Transport != agentoryv1alpha1.MCPTransportSSE {
		t.Errorf("SSE default path = %+v", sse)
	}
	for _, bad := range []string{"ftp://x/mcp", "not a url", "http://", "http://h:99999/mcp"} {
		if _, err := ParseURL(oncall, bad, ""); err == nil {
			t.Errorf("ParseURL(%q) succeeded, want error", bad)
		}
	}
}

func TestAuthorizationExpressions(t *testing.T) {
	got := AuthorizationExpressions([]Target{
		{Name: oncall, Tools: []string{"get_recent_deploys", "get_pod_events"}},
		{Name: "runbooks"},
	})
	want := []any{
		`mcp.tool.target == "oncall" && mcp.tool.name in ["get_recent_deploys", "get_pod_events"]`,
		`mcp.tool.target == "runbooks"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expressions = %v\nwant %v", got, want)
	}
}

func TestResolveLooksUpRegistryAfterOwnNamespace(t *testing.T) {
	ctx := context.Background()
	shared := server(registryNS, oncall, "http://shared:8080/mcp")
	local := server(testNamespace, oncall, "http://local:8080/mcp")

	m, _ := newManager(t, shared)
	targets, problem, err := m.Resolve(ctx, agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall}))
	if err != nil || problem != nil || targets[0].Host != "shared" {
		t.Fatalf("registry lookup = %+v, %+v, %v", targets, problem, err)
	}

	m, _ = newManager(t, shared, local)
	targets, _, _ = m.Resolve(ctx, agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall}))
	if targets[0].Host != "local" {
		t.Errorf("own namespace must win, got %s", targets[0].Host)
	}

	targets, _, _ = m.Resolve(ctx, agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall, URL: "http://direct:1/mcp"}))
	if targets[0].Host != "direct" {
		t.Errorf("url must override the registry, got %s", targets[0].Host)
	}
}

func TestResolveReportsProblems(t *testing.T) {
	m, _ := newManager(t, server(testNamespace, "broken", "http://"))
	_, problem, err := m.Resolve(context.Background(), agentWith(agentoryv1alpha1.MCPServerRef{Name: "missing"}))
	if err != nil || problem == nil || problem.Reason != ReasonMCPServerNotFound || !strings.Contains(problem.Message, registryNS) {
		t.Errorf("missing server = %+v, %v", problem, err)
	}
	_, problem, _ = m.Resolve(context.Background(), agentWith(agentoryv1alpha1.MCPServerRef{Name: "broken"}))
	if problem == nil || problem.Reason != ReasonInvalidMCPURL || !strings.Contains(problem.Message, "MCPServer team-payments/broken") {
		t.Errorf("bad url = %+v", problem)
	}
}

func TestEnsureCreatesBackendRouteAndPolicy(t *testing.T) {
	agent := agentWith(
		agentoryv1alpha1.MCPServerRef{Name: oncall, URL: oncallURL, Tools: []string{"get_recent_deploys"}},
		agentoryv1alpha1.MCPServerRef{Name: "saas", URL: "https://mcp.example.com/mcp"},
	)
	m, c := newManager(t, agent)
	st, err := m.Ensure(context.Background(), agent)
	if err != nil || st.Ready || st.Reason != ReasonToolsPending {
		t.Fatalf("Ensure = %+v, %v", st, err)
	}

	backend := get(t, c, a2a.BackendGVK)
	prefix, _, _ := unstructured.NestedString(backend.Object, "spec", "mcp", "prefixMode")
	targets, _, _ := unstructured.NestedSlice(backend.Object, "spec", "mcp", "targets")
	if prefix != "Always" || len(targets) != 2 {
		t.Fatalf("backend mcp = prefix %q, %d targets", prefix, len(targets))
	}
	first := targets[0].(map[string]any)["static"].(map[string]any)
	if first["host"] != "oncall-mcp.mcp-demo.svc.cluster.local" || first["port"] != int64(8080) || first["protocol"] != "StreamableHTTP" {
		t.Errorf("first target = %v", first)
	}
	if _, hasTLS := first["policies"]; hasTLS {
		t.Error("plain http target must not get TLS")
	}
	sni, _, _ := unstructured.NestedString(targets[1].(map[string]any), "static", "policies", "tls", "sni")
	if sni != "mcp.example.com" {
		t.Errorf("https target sni = %q", sni)
	}

	route := get(t, c, a2a.HTTPRouteGVK)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	prefixPath, _, _ := unstructured.NestedString(rules[0].(map[string]any)["matches"].([]any)[0].(map[string]any), "path", "value")
	if prefixPath != "/mcp/team-payments/payments-oncall" {
		t.Errorf("route path = %q", prefixPath)
	}

	policy := get(t, c, PolicyGVK)
	exprs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions")
	if len(exprs) != 2 || !strings.Contains(exprs[0].(string), `["get_recent_deploys"]`) {
		t.Errorf("policy expressions = %v", exprs)
	}
	for _, obj := range []*unstructured.Unstructured{backend, route, policy} {
		if !metav1.IsControlledBy(obj, agent) {
			t.Errorf("%s must be owned by the agent", obj.GetKind())
		}
	}
}

func TestEnsureReportsAcceptedRouteAndConflicts(t *testing.T) {
	agent := agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall, URL: oncallURL})
	m, c := newManager(t, agent)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, agent); err != nil {
		t.Fatal(err)
	}
	route := get(t, c, a2a.HTTPRouteGVK)
	_ = unstructured.SetNestedSlice(route.Object, []any{map[string]any{
		"parentRef": map[string]any{"name": "agentory-gateway"},
		"conditions": []any{
			map[string]any{"type": "Accepted", "status": "True"}, map[string]any{"type": "ResolvedRefs", "status": "True"}},
	}}, "status", "parents")
	if err := c.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	st, err := m.Ensure(ctx, agent)
	if err != nil || !st.Ready || !strings.Contains(st.Message, "/mcp/team-payments/payments-oncall") {
		t.Errorf("accepted route = %+v, %v", st, err)
	}

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(PolicyGVK)
	foreign.SetNamespace(testNamespace)
	foreign.SetName("other-mcp")
	other := agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall, URL: oncallURL})
	other.Name, other.UID = "other", "uid-2"
	m2, _ := newManager(t, other, foreign)
	if st, err := m2.Ensure(ctx, other); err != nil || st.Reason != ReasonResourceConflict {
		t.Errorf("foreign policy = %+v, %v; want ResourceConflict", st, err)
	}
}

func TestRemove(t *testing.T) {
	agent := agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall, URL: oncallURL})
	m, c := newManager(t, agent)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{a2a.HTTPRouteGVK, a2a.BackendGVK, PolicyGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: objectName}, u); err == nil {
			t.Errorf("%s left after Remove", gvk.Kind)
		}
	}
}

func TestHasToolsAndPath(t *testing.T) {
	if HasTools(agentWith()) {
		t.Error("an empty server list means no tools")
	}
	a := agentWith(agentoryv1alpha1.MCPServerRef{Name: oncall})
	if !HasTools(a) || Path(a) != "/mcp/team-payments/payments-oncall" || ObjectName(a) != objectName {
		t.Errorf("HasTools/Path/ObjectName = %v %s %s", HasTools(a), Path(a), ObjectName(a))
	}
}
