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

// Package runtime runs Agents on Kubernetes through pluggable backends
// (HLD-0001 §4, ADR-0001). The operator picks a Backend per Agent; each
// backend owns the Kubernetes objects it creates, so deleting an Agent
// garbage-collects them.
package runtime

import (
	"context"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// Labels set on every object a backend creates.
const (
	LabelAgent     = "agentory.dev/agent"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "agentory-operator"

	// AnnotationRevision on pod templates makes a spec change roll the workload.
	AnnotationRevision = "agentory.dev/revision"
	// AnnotationVirtualKeyID on pod templates makes a key rotation roll the workload,
	// since environment variables from Secrets are only read at container start.
	AnnotationVirtualKeyID = "agentory.dev/virtual-key-id"
)

// Deps is what the operator resolved for an Agent before handing it to a backend.
type Deps struct {
	// Revision is the Agent's spec revision.
	Revision string
	// VirtualKeyID identifies the Agent's LLM gateway key; empty if keys are not managed.
	VirtualKeyID string
}

// AgentRuntimeStatus is what a backend reports about an Agent's workload.
type AgentRuntimeStatus struct {
	// WorkloadReady is true when the workload is serving at the current revision.
	WorkloadReady bool
	// Reason is a CamelCase reason for the WorkloadReady condition.
	Reason string
	// Message explains the state to a human.
	Message string
	// InternalEndpoint is the in-cluster address of the workload, if it has one.
	InternalEndpoint string
	// ServingPort is the port the workload's Service listens on (0 if none).
	ServingPort int32
}

// Backend runs one kind of Agent. Implementations are idempotent: EnsureAgent
// is called on every reconcile and must converge, not duplicate.
//
// Task execution (StartTask/TaskStatus/CancelTask in HLD-0001 §4) is added with
// the first per-task backend (backlog item 7).
type Backend interface {
	// Name identifies the backend in conditions and logs.
	Name() string
	// Supports reports whether this backend can run the Agent.
	Supports(agent *agentoryv1alpha1.Agent) bool
	// EnsureAgent creates or updates the Agent's long-lived resources and reports their state.
	EnsureAgent(ctx context.Context, agent *agentoryv1alpha1.Agent, deps Deps) (AgentRuntimeStatus, error)
}

// Registry picks the backend for an Agent. The first backend that supports it wins.
type Registry struct {
	backends []Backend
}

// NewRegistry returns a registry over the given backends, in priority order.
func NewRegistry(backends ...Backend) *Registry {
	return &Registry{backends: backends}
}

// For returns the backend for the Agent, or false if none supports it yet.
func (r *Registry) For(agent *agentoryv1alpha1.Agent) (Backend, bool) {
	if r == nil {
		return nil, false
	}
	for _, b := range r.backends {
		if b.Supports(agent) {
			return b, true
		}
	}
	return nil, false
}

// SelectorLabels identify an Agent's pods.
func SelectorLabels(agent *agentoryv1alpha1.Agent) map[string]string {
	return map[string]string{LabelAgent: agent.Name}
}

// ObjectLabels are set on every object created for an Agent.
func ObjectLabels(agent *agentoryv1alpha1.Agent) map[string]string {
	return map[string]string{
		LabelAgent:                  agent.Name,
		LabelManagedBy:              ManagedBy,
		"app.kubernetes.io/name":    agent.Name,
		"app.kubernetes.io/part-of": "agentory",
	}
}

// VirtualKeySecretName is the Secret holding the Agent's LLM gateway virtual key
// (key "virtual-key"). It is created by backlog item 5; until then the
// environment references it as optional.
func VirtualKeySecretName(agent *agentoryv1alpha1.Agent) string {
	return agent.Name + "-llm"
}

// VirtualKeySecretKey is the key inside VirtualKeySecretName's Secret.
const VirtualKeySecretKey = "virtual-key"

// AgentCardConfigMapName is the ConfigMap holding the Agent's generated A2A card
// (key AgentCardFile). Workloads mount it at AgentCardMountPath; the volume is
// optional, so agents that are not exposed simply have no card file.
func AgentCardConfigMapName(agent *agentoryv1alpha1.Agent) string {
	return agent.Name + "-agent-card"
}

// Agent card file name and mount location inside agent containers.
const (
	AgentCardFile      = "agent-card.json"
	AgentCardMountPath = "/etc/agentory/agent-card"
)
