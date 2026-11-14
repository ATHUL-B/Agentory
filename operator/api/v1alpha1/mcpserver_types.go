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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MCPTransport is how the agent gateway talks to an MCP server.
// +kubebuilder:validation:Enum=StreamableHTTP;SSE
type MCPTransport string

const (
	MCPTransportStreamableHTTP MCPTransport = "StreamableHTTP"
	MCPTransportSSE            MCPTransport = "SSE"
)

// MCPServerSpec registers an MCP server that Agents can use by name.
// Agents look a name up in their own namespace first, then in the platform's
// shared registry namespace (operator flag --mcp-registry-namespace).
type MCPServerSpec struct {
	// URL of the server's MCP endpoint, e.g. http://oncall-mcp.mcp-demo.svc.cluster.local:8080/mcp.
	// +kubebuilder:validation:Pattern=`^https?://[^/:]+(:[0-9]+)?(/.*)?$`
	// +required
	URL string `json:"url"`

	// Transport defaults to StreamableHTTP.
	// +kubebuilder:default=StreamableHTTP
	// +optional
	Transport MCPTransport `json:"transport,omitempty"`

	// Description tells people (and the UI) what the server's tools are for.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Description string `json:"description,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mcp
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MCPServer is a named MCP server in the platform's tool registry.
type MCPServer struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec MCPServerSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer.
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &MCPServer{}, &MCPServerList{})
		return nil
	})
}
