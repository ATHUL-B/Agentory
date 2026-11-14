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

// Package a2a exposes Agents over A2A through the agent gateway (backlog item
// 6): a generated Agent Card, an agentgateway A2A backend and an HTTPRoute at
// /agents/<namespace>/<agent>.
package a2a

import (
	"fmt"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

// ProtocolVersion is the A2A protocol version of generated cards.
const ProtocolVersion = "0.3.0"

// CardPath is where A2A clients fetch an agent's card, relative to its URL.
const CardPath = "/.well-known/agent-card.json"

// Card is an A2A Agent Card (the subset Agentory generates).
type Card struct {
	ProtocolVersion    string       `json:"protocolVersion"`
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	URL                string       `json:"url"`
	PreferredTransport string       `json:"preferredTransport"`
	Version            string       `json:"version"`
	Capabilities       Capabilities `json:"capabilities"`
	DefaultInputModes  []string     `json:"defaultInputModes"`
	DefaultOutputModes []string     `json:"defaultOutputModes"`
	Skills             []Skill      `json:"skills"`
}

// Capabilities advertises optional protocol features.
type Capabilities struct {
	Streaming bool `json:"streaming"`
}

// Skill is one capability listed on the card.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Path is the agent's path on the agent gateway.
func Path(agent *agentoryv1alpha1.Agent) string {
	return fmt.Sprintf("/agents/%s/%s", agent.Namespace, agent.Name)
}

// URL is the agent's A2A endpoint on the agent gateway.
func URL(cfg agentruntime.GatewayConfig, agent *agentoryv1alpha1.Agent) string {
	return cfg.AgentGateway.URL(cfg.ClusterDomain, Path(agent))
}

// CardBuilder builds an Agent's card from its spec.
type CardBuilder struct {
	Config agentruntime.GatewayConfig
}

// Build returns the card for the Agent at the given revision. The URL is the
// gateway endpoint, so callers never learn the workload's address.
func (b CardBuilder) Build(agent *agentoryv1alpha1.Agent, revision string) Card {
	description := agent.Spec.Description
	if description == "" {
		description = fmt.Sprintf("Agentory agent %s/%s", agent.Namespace, agent.Name)
	}
	skills := []Skill{}
	if agent.Spec.Declarative != nil {
		for _, s := range agent.Spec.Declarative.Skills {
			skills = append(skills, Skill{ID: s.Name, Name: s.Name, Description: "Skill " + s.Name, Tags: []string{"skill"}})
		}
	}
	return Card{
		ProtocolVersion:    ProtocolVersion,
		Name:               agent.Name,
		Description:        description,
		URL:                URL(b.Config, agent),
		PreferredTransport: "JSONRPC",
		Version:            revision,
		Capabilities:       Capabilities{Streaming: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             skills,
	}
}
