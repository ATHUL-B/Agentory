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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Runtime selects what executes the agent (HLD-0001 §5).
// +kubebuilder:validation:Enum=harness;container;declarative
type Runtime string

const (
	// RuntimeHarness runs a coding harness such as Claude Code or Codex.
	RuntimeHarness Runtime = "harness"
	// RuntimeContainer runs a user-supplied container image.
	RuntimeContainer Runtime = "container"
	// RuntimeDeclarative runs Agentory's own runtime from instructions, skills and MCP tools.
	RuntimeDeclarative Runtime = "declarative"
)

// Lifecycle selects how long the agent's workload lives.
// +kubebuilder:validation:Enum=perTask;service;suspendable
type Lifecycle string

const (
	// LifecyclePerTask starts a workload for each Task and removes it afterwards.
	LifecyclePerTask Lifecycle = "perTask"
	// LifecycleService keeps a long-lived workload serving A2A requests.
	LifecycleService Lifecycle = "service"
	// LifecycleSuspendable is reserved for the Agent Substrate backend (backlog item 18).
	LifecycleSuspendable Lifecycle = "suspendable"
)

// AgentSpec defines the desired state of an Agent.
// +kubebuilder:validation:XValidation:rule="self.runtime != 'declarative' || (has(self.declarative) && !has(self.harness) && !has(self.container))",message="runtime 'declarative' requires spec.declarative and no spec.harness or spec.container"
// +kubebuilder:validation:XValidation:rule="self.runtime != 'harness' || (has(self.harness) && !has(self.declarative) && !has(self.container))",message="runtime 'harness' requires spec.harness and no spec.declarative or spec.container"
// +kubebuilder:validation:XValidation:rule="self.runtime != 'container' || (has(self.container) && !has(self.declarative) && !has(self.harness))",message="runtime 'container' requires spec.container and no spec.declarative or spec.harness"
// +kubebuilder:validation:XValidation:rule="self.runtime != 'harness' || self.lifecycle == 'perTask'",message="runtime 'harness' requires lifecycle 'perTask'"
// +kubebuilder:validation:XValidation:rule="self.lifecycle != 'suspendable'",message="lifecycle 'suspendable' is not supported yet (needs the Agent Substrate backend, backlog item 18)"
type AgentSpec struct {
	// Description is a human-readable summary, published in the A2A Agent Card.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Description string `json:"description,omitempty"`

	// Runtime selects what executes the agent.
	// +required
	Runtime Runtime `json:"runtime"`

	// Lifecycle selects how long the workload lives.
	// +required
	Lifecycle Lifecycle `json:"lifecycle"`

	// Declarative configures Agentory's own runtime. Required when runtime is "declarative".
	// +optional
	Declarative *DeclarativeSpec `json:"declarative,omitempty"`

	// Harness configures a coding harness. Required when runtime is "harness".
	// +optional
	Harness *HarnessSpec `json:"harness,omitempty"`

	// Container configures a user-supplied image. Required when runtime is "container".
	// +optional
	Container *ContainerSpec `json:"container,omitempty"`

	// Model is the model the agent calls through the LLM gateway.
	// +required
	Model ModelSpec `json:"model"`

	// Tools lists the MCP servers the agent may use, reached through the agent gateway.
	// +optional
	Tools *ToolsSpec `json:"tools,omitempty"`

	// A2A controls whether and to whom the agent is exposed over A2A.
	// +optional
	A2A *A2ASpec `json:"a2a,omitempty"`

	// Observability selects the trace sink. Defaults to the cluster default.
	// +optional
	Observability *ObservabilitySpec `json:"observability,omitempty"`

	// Resources for the agent's workload.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// DeclarativeSpec configures a declarative (skills + MCP) agent.
type DeclarativeSpec struct {
	// Instructions is the system prompt.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +required
	Instructions string `json:"instructions"`

	// Skills are loaded from immutable sources.
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	// +optional
	Skills []SkillRef `json:"skills,omitempty"`
}

// SkillRef points at one skill (a SKILL.md bundle) at an immutable version.
type SkillRef struct {
	// Name identifies the skill within the agent.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// Source is where the skill is fetched from.
	// +required
	Source ArtifactSource `json:"source"`
}

// ArtifactSource selects exactly one immutable artifact location.
// +kubebuilder:validation:XValidation:rule="has(self.git) != has(self.oci)",message="exactly one of git or oci must be set"
type ArtifactSource struct {
	// +optional
	Git *GitArtifact `json:"git,omitempty"`
	// +optional
	OCI *OCIArtifact `json:"oci,omitempty"`
}

// GitArtifact is content at a full commit SHA, so it cannot change underneath the agent.
type GitArtifact struct {
	// +kubebuilder:validation:Pattern=`^(https|ssh)://.+`
	// +required
	URL string `json:"url"`

	// Commit is a full 40-character commit SHA (branches and tags are mutable).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	Commit string `json:"commit"`

	// Path inside the repository.
	// +optional
	Path string `json:"path,omitempty"`
}

// OCIArtifact is an OCI artifact pinned by digest.
type OCIArtifact struct {
	// Ref must include a sha256 digest, e.g. registry/skills/triage@sha256:...
	// +kubebuilder:validation:Pattern=`^.+@sha256:[0-9a-f]{64}$`
	// +required
	Ref string `json:"ref"`
}

// HarnessKind names a supported coding harness.
// +kubebuilder:validation:Enum=claude-code;codex
type HarnessKind string

// Harness kinds.
const (
	HarnessKindClaudeCode HarnessKind = "claude-code"
	HarnessKindCodex      HarnessKind = "codex"
)

// HarnessSpec configures a coding harness agent.
type HarnessSpec struct {
	// Kind selects the harness.
	// +required
	Kind HarnessKind `json:"kind"`

	// Image overrides the default harness image.
	// +optional
	Image string `json:"image,omitempty"`

	// Workspace is the repository checked out for each task.
	// +optional
	Workspace *WorkspaceSpec `json:"workspace,omitempty"`
}

// WorkspaceSpec describes the working tree a harness operates on.
type WorkspaceSpec struct {
	// +required
	Git GitWorkspace `json:"git"`
}

// GitWorkspace is a repository and ref to check out.
type GitWorkspace struct {
	// +kubebuilder:validation:Pattern=`^(https|ssh)://.+`
	// +required
	URL string `json:"url"`

	// Ref is a branch, tag or commit. Defaults to the repository's default branch.
	// +optional
	Ref string `json:"ref,omitempty"`
}

// ContainerProtocol is how the platform talks to a container agent.
// +kubebuilder:validation:Enum=a2a;http
type ContainerProtocol string

// ContainerSpec configures a user-supplied agent image.
type ContainerSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// Port the container serves on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// Protocol is "a2a" (routed directly) or "http" (wrapped by an A2A adapter, backlog item 26).
	// +kubebuilder:default=a2a
	// +optional
	Protocol ContainerProtocol `json:"protocol,omitempty"`

	// Env adds environment variables. Names the platform injects (AGENTORY_*, OTEL_*,
	// and the OpenAI/Anthropic SDK base URL and key variables) are reserved.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.name.startsWith('AGENTORY_') && !e.name.startsWith('OTEL_') && !(e.name in ['OPENAI_BASE_URL', 'OPENAI_API_KEY', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_API_KEY']))",message="env names AGENTORY_*, OTEL_*, OPENAI_BASE_URL, OPENAI_API_KEY, ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY are reserved for the platform"
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// ModelSpec names a model as the LLM gateway knows it.
type ModelSpec struct {
	// Provider as configured in the LLM gateway, e.g. "anthropic", "openai" or "mock".
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Provider string `json:"provider"`

	// Name of the model at that provider, e.g. "claude-sonnet-5-5".
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// ToolsSpec lists the MCP servers an agent may use.
type ToolsSpec struct {
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	MCPServers []MCPServerRef `json:"mcpServers,omitempty"`
}

// MCPServerRef is a platform-registered MCP server (by name) or a user-supplied endpoint (url).
type MCPServerRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// URL of a user-supplied MCP endpoint. Omit for a platform-registered server.
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +optional
	URL string `json:"url,omitempty"`

	// Tools is an allow-list of tool names. Empty allows all tools of the server.
	// +listType=set
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// A2ASpec controls A2A exposure.
type A2ASpec struct {
	// Expose registers an A2A route and Agent Card on the agent gateway.
	// +kubebuilder:default=true
	// +optional
	Expose *bool `json:"expose,omitempty"`

	// AllowedCallers are "namespace/agent" patterns ("*" wildcards allowed) permitted to call this agent.
	// +listType=set
	// +optional
	AllowedCallers []string `json:"allowedCallers,omitempty"`
}

// TraceSink selects where traces go.
// +kubebuilder:validation:Enum=default;langfuse;mlflow;none
type TraceSink string

// ObservabilitySpec configures tracing for the agent.
type ObservabilitySpec struct {
	// +kubebuilder:default=default
	// +optional
	Sink TraceSink `json:"sink,omitempty"`
}

// AgentPhase is a coarse summary of the Agent's state.
// +kubebuilder:validation:Enum=Pending;Ready;Degraded;Failed
type AgentPhase string

const (
	AgentPhasePending  AgentPhase = "Pending"
	AgentPhaseReady    AgentPhase = "Ready"
	AgentPhaseDegraded AgentPhase = "Degraded"
	AgentPhaseFailed   AgentPhase = "Failed"
)

// AgentEndpoints are where clients reach the agent.
type AgentEndpoints struct {
	// Internal is the in-cluster address of the agent's workload. Its network policy only
	// admits traffic from the agent gateway; other agents should use the A2A endpoint.
	// +optional
	Internal string `json:"internal,omitempty"`
	// +optional
	A2A string `json:"a2a,omitempty"`
	// +optional
	AgentCard string `json:"agentCard,omitempty"`
}

// AgentStatus defines the observed state of an Agent.
type AgentStatus struct {
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// ObservedGeneration is the generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Revision is a hash of the spec; Tasks record the revision they ran against.
	// +optional
	Revision string `json:"revision,omitempty"`

	// +optional
	Endpoints *AgentEndpoints `json:"endpoints,omitempty"`

	// VirtualKeyRef names the Secret holding the agent's LLM gateway virtual key.
	// +optional
	VirtualKeyRef *corev1.LocalObjectReference `json:"virtualKeyRef,omitempty"`

	// Conditions: Accepted, WorkloadReady, GatewayRouteReady, VirtualKeyReady, EgressPolicyApplied, Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag;agents
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Lifecycle",type=string,JSONPath=`.spec.lifecycle`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the Schema for the agents API.
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec AgentSpec `json:"spec"`

	// +optional
	Status AgentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Agent{}, &AgentList{})
		return nil
	})
}
