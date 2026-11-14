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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TaskSpec defines a durable, auditable unit of work (HLD-0001 §3.2).
// The spec is immutable: a Task records what was asked; ask again with a new Task.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Task spec is immutable; create a new Task instead"
type TaskSpec struct {
	// AgentRef names an Agent in the same namespace.
	// +required
	AgentRef AgentReference `json:"agentRef"`

	// Input is what the agent is asked to do.
	// +required
	Input TaskInput `json:"input"`

	// Timeout bounds the task's run time.
	// +kubebuilder:default="30m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// AgentReference names an Agent in the same namespace.
type AgentReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
}

// TaskInput is the request sent to the agent.
type TaskInput struct {
	// Message is the natural-language instruction.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +required
	Message string `json:"message"`

	// Data is optional structured context (any JSON object).
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Data *apiextensionsv1.JSON `json:"data,omitempty"`
}

// TaskPhase summarises where a Task is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;Running;AwaitingInput;Succeeded;Failed;Cancelled
type TaskPhase string

const (
	TaskPhasePending       TaskPhase = "Pending"
	TaskPhaseRunning       TaskPhase = "Running"
	TaskPhaseAwaitingInput TaskPhase = "AwaitingInput"
	TaskPhaseSucceeded     TaskPhase = "Succeeded"
	TaskPhaseFailed        TaskPhase = "Failed"
	TaskPhaseCancelled     TaskPhase = "Cancelled"
)

// IsTerminal reports whether the phase is final.
func (p TaskPhase) IsTerminal() bool {
	return p == TaskPhaseSucceeded || p == TaskPhaseFailed || p == TaskPhaseCancelled
}

// TaskUsage is model usage attributed to the task.
type TaskUsage struct {
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CostUSD is a decimal string (e.g. "0.31"); floats are avoided in Kubernetes APIs.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	CostUSD string `json:"costUsd,omitempty"`
}

// TaskArtifact is something the task produced, e.g. a git branch.
type TaskArtifact struct {
	// +required
	Type string `json:"type"`
	// +required
	Ref string `json:"ref"`
}

// TaskResult is the outcome of a finished task.
type TaskResult struct {
	// +optional
	Summary string `json:"summary,omitempty"`
	// +optional
	Artifacts []TaskArtifact `json:"artifacts,omitempty"`
}

// TaskStatus defines the observed state of a Task.
type TaskStatus struct {
	// +optional
	Phase TaskPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// AgentRevision is the Agent revision this task ran (or will run) against.
	// +optional
	AgentRevision string `json:"agentRevision,omitempty"`

	// TraceID links the task to its trace in the trace sink.
	// +optional
	TraceID string `json:"traceId,omitempty"`

	// +optional
	Usage *TaskUsage `json:"usage,omitempty"`

	// +optional
	Result *TaskResult `json:"result,omitempty"`

	// Conditions: AgentResolved, Started.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tk
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Trace",type=string,JSONPath=`.status.traceId`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Task is the Schema for the tasks API.
type Task struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TaskSpec `json:"spec"`

	// +optional
	Status TaskStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task.
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Task{}, &TaskList{})
		return nil
	})
}
