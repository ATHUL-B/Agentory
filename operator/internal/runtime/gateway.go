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
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// ServiceRef locates an in-cluster Service.
type ServiceRef struct {
	Namespace string
	Name      string
	Port      int32
}

// Host returns the Service's cluster DNS name.
func (s ServiceRef) Host(clusterDomain string) string {
	return fmt.Sprintf("%s.%s.svc.%s", s.Name, s.Namespace, clusterDomain)
}

// URL returns http://host:port followed by path.
func (s ServiceRef) URL(clusterDomain, path string) string {
	return fmt.Sprintf("http://%s:%d%s", s.Host(clusterDomain), s.Port, path)
}

// GatewayConfig describes the platform services every agent is wired to (ADR-0002).
type GatewayConfig struct {
	ClusterDomain string

	// LLMGateway is Bifrost. Agents reach models only through it.
	LLMGateway ServiceRef
	// AgentGateway is agentgateway. Agents reach MCP tools and other agents only through it.
	AgentGateway ServiceRef
	// Collector is the OTel collector (OTLP/HTTP port).
	Collector ServiceRef
	// CollectorGRPCPort is the collector's OTLP/gRPC port, allowed alongside Collector.Port.
	CollectorGRPCPort int32
	// CollectorPodLabels select the collector pods in network policy.
	CollectorPodLabels map[string]string

	// DNSNamespace and DNSPodLabels select cluster DNS in network policy.
	DNSNamespace string
	DNSPodLabels map[string]string
}

// DefaultGatewayConfig matches the local dev cluster (deploy/local).
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		ClusterDomain:      "cluster.local",
		LLMGateway:         ServiceRef{Namespace: "bifrost", Name: "bifrost", Port: 8080},
		AgentGateway:       ServiceRef{Namespace: "agentgateway-system", Name: "agentory-gateway", Port: 8080},
		Collector:          ServiceRef{Namespace: "observability", Name: "otel-collector", Port: 4318},
		CollectorGRPCPort:  4317,
		CollectorPodLabels: map[string]string{"app.kubernetes.io/name": "opentelemetry-collector"},
		DNSNamespace:       "kube-system",
		DNSPodLabels:       map[string]string{"k8s-app": "kube-dns"},
	}
}

// Platform environment variable names injected into every agent workload (HLD-0001 §6).
const (
	EnvAgent          = "AGENTORY_AGENT"
	EnvNamespace      = "AGENTORY_NAMESPACE"
	EnvRevision       = "AGENTORY_REVISION"
	EnvModel          = "AGENTORY_MODEL"
	EnvLLMBaseURL     = "AGENTORY_LLM_BASE_URL"
	EnvLLMAPIKey      = "AGENTORY_LLM_API_KEY"
	EnvA2AURL         = "AGENTORY_A2A_URL"
	EnvMCPURL         = "AGENTORY_MCP_URL"
	EnvAgentCardPath  = "AGENTORY_AGENT_CARD_PATH"
	EnvOpenAIBaseURL  = "OPENAI_BASE_URL"
	EnvOpenAIAPIKey   = "OPENAI_API_KEY"
	EnvAnthropicURL   = "ANTHROPIC_BASE_URL"
	EnvAnthropicKey   = "ANTHROPIC_API_KEY"
	EnvOTLPEndpoint   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPProtocol   = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvOTelService    = "OTEL_SERVICE_NAME"
	EnvOTelAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	EnvOTelDisabled   = "OTEL_SDK_DISABLED"
)

// EnvBuilder produces the platform environment for an agent workload, so any
// agent, including a black-box container, talks to models, tools and tracing
// through the platform without knowing where they are.
type EnvBuilder struct {
	Config GatewayConfig
}

// Build returns the platform variables for the Agent at the given revision.
func (b EnvBuilder) Build(agent *agentoryv1alpha1.Agent, revision string) []corev1.EnvVar {
	cfg := b.Config
	llm := func(path string) string { return cfg.LLMGateway.URL(cfg.ClusterDomain, path) }
	agw := func(path string) string { return cfg.AgentGateway.URL(cfg.ClusterDomain, path) }
	key := corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: VirtualKeySecretName(agent)},
		Key:                  VirtualKeySecretKey,
		Optional:             new(true),
	}}

	env := make([]corev1.EnvVar, 0, 16)
	env = append(env,
		corev1.EnvVar{Name: EnvAgent, Value: agent.Name},
		corev1.EnvVar{Name: EnvNamespace, Value: agent.Namespace},
		corev1.EnvVar{Name: EnvRevision, Value: revision},
		corev1.EnvVar{Name: EnvModel, Value: agent.Spec.Model.Provider + "/" + agent.Spec.Model.Name},
		// Bifrost's OpenAI-compatible API, plus the paths its SDK integrations serve.
		corev1.EnvVar{Name: EnvLLMBaseURL, Value: llm("/v1")},
		corev1.EnvVar{Name: EnvOpenAIBaseURL, Value: llm("/openai")},
		corev1.EnvVar{Name: EnvAnthropicURL, Value: llm("/anthropic")},
		// The same virtual key under each SDK's variable; Bifrost reads it from
		// x-api-key (Anthropic SDK) or Authorization (OpenAI SDK).
		corev1.EnvVar{Name: EnvLLMAPIKey, ValueFrom: key.DeepCopy()},
		corev1.EnvVar{Name: EnvOpenAIAPIKey, ValueFrom: key.DeepCopy()},
		corev1.EnvVar{Name: EnvAnthropicKey, ValueFrom: key.DeepCopy()},
		corev1.EnvVar{Name: EnvA2AURL, Value: agw("")},
		corev1.EnvVar{Name: EnvMCPURL, Value: agw(fmt.Sprintf("/mcp/%s/%s", agent.Namespace, agent.Name))},
		corev1.EnvVar{Name: EnvAgentCardPath, Value: AgentCardMountPath + "/" + AgentCardFile},
	)
	return append(env, b.telemetryEnv(agent, revision)...)
}

func (b EnvBuilder) telemetryEnv(agent *agentoryv1alpha1.Agent, revision string) []corev1.EnvVar {
	if agent.Spec.Observability != nil && agent.Spec.Observability.Sink == "none" {
		return []corev1.EnvVar{{Name: EnvOTelDisabled, Value: "true"}}
	}
	attrs := strings.Join([]string{
		"agentory.agent=" + agent.Name,
		"agentory.namespace=" + agent.Namespace,
		"agentory.revision=" + revision,
	}, ",")
	return []corev1.EnvVar{
		{Name: EnvOTLPEndpoint, Value: b.Config.Collector.URL(b.Config.ClusterDomain, "")},
		{Name: EnvOTLPProtocol, Value: "http/protobuf"},
		{Name: EnvOTelService, Value: agent.Name},
		{Name: EnvOTelAttributes, Value: attrs},
	}
}

// ParseServiceRef parses "namespace/name:port", the format of the operator's
// gateway flags.
func ParseServiceRef(s string) (ServiceRef, error) {
	nsName, portStr, ok := strings.Cut(s, ":")
	ns, name, ok2 := strings.Cut(nsName, "/")
	if !ok || !ok2 || ns == "" || name == "" {
		return ServiceRef{}, fmt.Errorf("service %q: want namespace/name:port", s)
	}
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return ServiceRef{}, fmt.Errorf("service %q: invalid port %q", s, portStr)
	}
	return ServiceRef{Namespace: ns, Name: name, Port: int32(port)}, nil
}

// String formats the ref as "namespace/name:port".
func (s ServiceRef) String() string {
	return fmt.Sprintf("%s/%s:%d", s.Namespace, s.Name, s.Port)
}
