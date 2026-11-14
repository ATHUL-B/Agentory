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

package runtime

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const (
	testAgent     = "crew"
	testNamespace = "team-a"
)

func containerAgent(mutate ...func(*agentoryv1alpha1.Agent)) *agentoryv1alpha1.Agent {
	a := &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testAgent, Namespace: testNamespace, UID: "uid-1"},
		Spec: agentoryv1alpha1.AgentSpec{
			Runtime:   agentoryv1alpha1.RuntimeContainer,
			Lifecycle: agentoryv1alpha1.LifecycleService,
			Container: &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/crew:1.0.0", Port: 9000},
			Model:     agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
		},
	}
	for _, m := range mutate {
		m(a)
	}
	return a
}

func envMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

func TestEnvBuilderWiresGatewaysAndTelemetry(t *testing.T) {
	env := envMap(EnvBuilder{Config: DefaultGatewayConfig()}.Build(containerAgent(), testAgent+"-abc"))

	want := map[string]string{
		EnvAgent:          testAgent,
		EnvNamespace:      testNamespace,
		EnvRevision:       testAgent + "-abc",
		EnvModel:          "mock/mock-model",
		EnvLLMBaseURL:     "http://bifrost.bifrost.svc.cluster.local:8080/v1",
		EnvOpenAIBaseURL:  "http://bifrost.bifrost.svc.cluster.local:8080/openai",
		EnvAnthropicURL:   "http://bifrost.bifrost.svc.cluster.local:8080/anthropic",
		EnvA2AURL:         "http://agentory-gateway.agentgateway-system.svc.cluster.local:8080",
		EnvMCPURL:         "http://agentory-gateway.agentgateway-system.svc.cluster.local:8080/mcp/team-a/crew",
		EnvOTLPEndpoint:   "http://otel-collector.observability.svc.cluster.local:4318",
		EnvOTLPProtocol:   "http/protobuf",
		EnvOTelService:    testAgent,
		EnvOTelAttributes: "agentory.agent=crew,agentory.namespace=team-a,agentory.revision=crew-abc",
	}
	for name, value := range want {
		if got := env[name].Value; got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if _, ok := env[EnvOTelDisabled]; ok {
		t.Errorf("%s must not be set when tracing is on", EnvOTelDisabled)
	}
}

func TestEnvBuilderKeysComeFromOptionalSecret(t *testing.T) {
	env := envMap(EnvBuilder{Config: DefaultGatewayConfig()}.Build(containerAgent(), "r"))
	for _, name := range []string{EnvLLMAPIKey, EnvOpenAIAPIKey, EnvAnthropicKey} {
		e := env[name]
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must come from a secret, got %+v", name, e)
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != testAgent+"-llm" || ref.Key != VirtualKeySecretKey || ref.Optional == nil || !*ref.Optional {
			t.Errorf("%s secret ref = %+v, want optional crew-llm/%s", name, ref, VirtualKeySecretKey)
		}
	}
}

func TestEnvBuilderSinkNoneDisablesTelemetry(t *testing.T) {
	agent := containerAgent(func(a *agentoryv1alpha1.Agent) {
		a.Spec.Observability = &agentoryv1alpha1.ObservabilitySpec{Sink: "none"}
	})
	env := envMap(EnvBuilder{Config: DefaultGatewayConfig()}.Build(agent, "r"))
	if env[EnvOTelDisabled].Value != "true" {
		t.Errorf("%s = %q, want true", EnvOTelDisabled, env[EnvOTelDisabled].Value)
	}
	if _, ok := env[EnvOTLPEndpoint]; ok {
		t.Errorf("%s must not be set when the sink is none", EnvOTLPEndpoint)
	}
}

func TestPolicyBuilderLocksDownEgressAndIngress(t *testing.T) {
	cfg := DefaultGatewayConfig()
	spec := PolicyBuilder{Config: cfg}.Build(containerAgent(), 9000)

	if spec.PodSelector.MatchLabels[LabelAgent] != testAgent {
		t.Fatalf("pod selector = %v", spec.PodSelector.MatchLabels)
	}
	if len(spec.PolicyTypes) != 2 {
		t.Fatalf("policy types = %v, want Ingress and Egress", spec.PolicyTypes)
	}

	type dest struct {
		namespace string
		ports     []int32
	}
	got := make([]dest, 0, len(spec.Egress))
	for _, rule := range spec.Egress {
		ns := rule.To[0].NamespaceSelector.MatchLabels[labelNamespaceName]
		ports := make([]int32, 0, len(rule.Ports))
		for _, p := range rule.Ports {
			ports = append(ports, p.Port.IntVal)
		}
		got = append(got, dest{ns, ports})
	}
	want := []dest{
		{"kube-system", []int32{53, 53}},
		{"bifrost", []int32{8080}},
		{"agentgateway-system", []int32{8080}},
		{"observability", []int32{4318, 4317}},
	}
	if len(got) != len(want) {
		t.Fatalf("egress rules = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].namespace != want[i].namespace || len(got[i].ports) != len(want[i].ports) {
			t.Fatalf("egress rule %d = %+v, want %+v", i, got[i], want[i])
		}
		for j := range want[i].ports {
			if got[i].ports[j] != want[i].ports[j] {
				t.Fatalf("egress rule %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}
	if spec.Egress[3].To[0].PodSelector == nil {
		t.Error("collector rule must also select the collector pods")
	}

	if len(spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want 1", len(spec.Ingress))
	}
	in := spec.Ingress[0]
	if in.From[0].NamespaceSelector.MatchLabels[labelNamespaceName] != "agentgateway-system" || in.Ports[0].Port.IntVal != 9000 {
		t.Errorf("ingress = %+v, want only agentgateway-system on 9000", in)
	}
}

func TestPolicyBuilderNoServingPortMeansNoIngress(t *testing.T) {
	spec := PolicyBuilder{Config: DefaultGatewayConfig()}.Build(containerAgent(), 0)
	if spec.Ingress == nil || len(spec.Ingress) != 0 {
		t.Fatalf("ingress = %#v, want an empty (deny-all) list", spec.Ingress)
	}
	hasIngress := false
	for _, pt := range spec.PolicyTypes {
		hasIngress = hasIngress || pt == networkingv1.PolicyTypeIngress
	}
	if !hasIngress {
		t.Error("policy must still declare Ingress so all ingress is denied")
	}
}

func TestParseServiceRef(t *testing.T) {
	ok, err := ParseServiceRef("llm/bifrost:9090")
	if err != nil || ok != (ServiceRef{Namespace: "llm", Name: "bifrost", Port: 9090}) {
		t.Fatalf("ParseServiceRef = %+v, %v", ok, err)
	}
	if ok.String() != "llm/bifrost:9090" {
		t.Errorf("String() = %q", ok.String())
	}
	for _, bad := range []string{"", "bifrost:8080", "llm/:8080", "/bifrost:8080", "llm/bifrost", "llm/bifrost:0", "llm/bifrost:99999", "llm/bifrost:x"} {
		if _, err := ParseServiceRef(bad); err == nil {
			t.Errorf("ParseServiceRef(%q) succeeded, want error", bad)
		}
	}
}

type fakeBackend struct {
	name string
	ok   bool
}

func (f fakeBackend) Name() string                          { return f.name }
func (f fakeBackend) Supports(*agentoryv1alpha1.Agent) bool { return f.ok }
func (f fakeBackend) EnsureAgent(_ context.Context, _ *agentoryv1alpha1.Agent, _ Deps) (AgentRuntimeStatus, error) {
	return AgentRuntimeStatus{}, nil
}

func TestRegistryPicksFirstSupportingBackend(t *testing.T) {
	reg := NewRegistry(fakeBackend{"no", false}, fakeBackend{"first", true}, fakeBackend{"second", true})
	b, ok := reg.For(containerAgent())
	if !ok || b.Name() != "first" {
		t.Fatalf("For() = %v, %v; want first", b, ok)
	}
	if _, ok := NewRegistry(fakeBackend{"no", false}).For(containerAgent()); ok {
		t.Error("For() found a backend when none supports the agent")
	}
	var nilRegistry *Registry
	if _, ok := nilRegistry.For(containerAgent()); ok {
		t.Error("nil registry must support nothing")
	}
}

func TestDeploymentBackendSupports(t *testing.T) {
	b := &DeploymentBackend{}
	cases := map[string]struct {
		agent *agentoryv1alpha1.Agent
		want  bool
	}{
		"container service": {containerAgent(), true},
		"container perTask": {containerAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.Lifecycle = agentoryv1alpha1.LifecyclePerTask }), false},
		"declarative service": {containerAgent(func(a *agentoryv1alpha1.Agent) {
			a.Spec.Runtime = agentoryv1alpha1.RuntimeDeclarative
			a.Spec.Container = nil
		}), false},
	}
	for name, tc := range cases {
		if got := b.Supports(tc.agent); got != tc.want {
			t.Errorf("%s: Supports = %v, want %v", name, got, tc.want)
		}
	}
}

func TestDeploymentStatus(t *testing.T) {
	deploy := func(gen, observed int64, updated, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: testAgent, Generation: gen},
			Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: observed, UpdatedReplicas: updated, AvailableReplicas: available,
			},
		}
	}
	cases := map[string]struct {
		d     *appsv1.Deployment
		ready bool
	}{
		"available at current generation": {deploy(2, 2, 1, 1), true},
		"not yet observed":                {deploy(2, 1, 1, 1), false},
		"rolling out":                     {deploy(2, 2, 0, 1), false},
		"no available pods":               {deploy(2, 2, 1, 0), false},
		"just created":                    {deploy(0, 0, 0, 0), false},
	}
	for name, tc := range cases {
		got := deploymentStatus(tc.d)
		if got.WorkloadReady != tc.ready {
			t.Errorf("%s: ready = %v, want %v (%s)", name, got.WorkloadReady, tc.ready, got.Message)
		}
		wantReason := ReasonProgressing
		if tc.ready {
			wantReason = ReasonAvailable
		}
		if got.Reason != wantReason {
			t.Errorf("%s: reason = %q, want %q", name, got.Reason, wantReason)
		}
	}
}
