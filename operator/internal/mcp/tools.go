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

// Package mcp gives Agents their MCP tools through the agent gateway (backlog
// 6b). Per Agent with tools.mcpServers, the operator owns an agentgateway MCP
// backend (one target per server), an HTTPRoute at /mcp/<namespace>/<agent>
// and an authorization policy built from each server's tools allow-list, so
// the gateway filters tools/list and rejects calls to tools that aren't allowed.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/a2a"
	"github.com/ATHUL-B/Agentory/operator/internal/ownership"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

// PolicyGVK is agentgateway's policy kind (backends and routes come from package a2a).
var PolicyGVK = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayPolicy"}

// fieldName is the "name" key used throughout the unstructured objects.
const fieldName = "name"

// Condition reasons for ToolsReady.
const (
	ReasonToolsRouted       = "ToolsRouted"
	ReasonToolsPending      = "ToolsPending"
	ReasonToolsRejected     = "ToolsRejected"
	ReasonMCPServerNotFound = "MCPServerNotFound"
	ReasonInvalidMCPURL     = "InvalidMCPURL"
	ReasonResourceConflict  = "ResourceConflict"
)

// ToolsStatus reports an Agent's MCP tools.
type ToolsStatus struct {
	Ready   bool
	Reason  string
	Message string
}

// Target is one resolved MCP server.
type Target struct {
	Name      string
	Host      string
	Port      int64
	Path      string
	TLS       bool
	Transport agentoryv1alpha1.MCPTransport
	Tools     []string // allow-list; empty allows all
}

// ObjectName is the name of the Agent's MCP backend, route and policy.
func ObjectName(agent *agentoryv1alpha1.Agent) string {
	return agent.Name + "-mcp"
}

// Path is the Agent's MCP endpoint on the agent gateway (AGENTORY_MCP_URL).
func Path(agent *agentoryv1alpha1.Agent) string {
	return fmt.Sprintf("/mcp/%s/%s", agent.Namespace, agent.Name)
}

// HasTools reports whether the Agent lists any MCP servers.
func HasTools(agent *agentoryv1alpha1.Agent) bool {
	return agent.Spec.Tools != nil && len(agent.Spec.Tools.MCPServers) > 0
}

// ParseURL turns an MCP endpoint URL into a gateway target.
func ParseURL(name, raw string, transport agentoryv1alpha1.MCPTransport) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Target{}, fmt.Errorf("MCP server %q: %q is not an http(s) URL", name, raw)
	}
	t := Target{Name: name, Host: u.Hostname(), TLS: u.Scheme == "https", Path: u.EscapedPath(), Transport: transport}
	if t.Transport == "" {
		t.Transport = agentoryv1alpha1.MCPTransportStreamableHTTP
	}
	switch {
	case u.Port() != "":
		if t.Port, err = strconv.ParseInt(u.Port(), 10, 32); err != nil || t.Port < 1 || t.Port > 65535 {
			return Target{}, fmt.Errorf("MCP server %q: invalid port in %q", name, raw)
		}
	case t.TLS:
		t.Port = 443
	default:
		t.Port = 80
	}
	if t.Path == "" || t.Path == "/" {
		t.Path = "/mcp"
		if t.Transport == agentoryv1alpha1.MCPTransportSSE {
			t.Path = "/sse"
		}
	}
	return t, nil
}

// AuthorizationExpressions are the CEL rules that allow exactly the listed tools
// (all of a server's tools when its list is empty). agentgateway evaluates
// mcp.tool.target (server) and mcp.tool.name (unprefixed tool name).
func AuthorizationExpressions(targets []Target) []any {
	exprs := make([]any, 0, len(targets))
	for _, t := range targets {
		rule := fmt.Sprintf("mcp.tool.target == %q", t.Name)
		if len(t.Tools) > 0 {
			quoted := make([]string, len(t.Tools))
			for i, tool := range t.Tools {
				quoted[i] = strconv.Quote(tool)
			}
			rule += fmt.Sprintf(" && mcp.tool.name in [%s]", strings.Join(quoted, ", "))
		}
		exprs = append(exprs, rule)
	}
	return exprs
}

// Manager maintains Agents' MCP tool routes.
type Manager struct {
	Client client.Client
	Scheme *k8sruntime.Scheme
	Config agentruntime.GatewayConfig
	// RegistryNamespace holds platform-wide MCPServers, searched after the Agent's namespace.
	RegistryNamespace string
}

// NewManager returns a manager attaching tool routes to cfg's agent gateway.
func NewManager(c client.Client, scheme *k8sruntime.Scheme, cfg agentruntime.GatewayConfig, registryNamespace string) *Manager {
	return &Manager{Client: c, Scheme: scheme, Config: cfg, RegistryNamespace: registryNamespace}
}

// Resolve turns the Agent's mcpServers into gateway targets. A problem the
// author must fix (unknown server, bad URL) is returned as a status, not an error.
func (m *Manager) Resolve(ctx context.Context, agent *agentoryv1alpha1.Agent) ([]Target, *ToolsStatus, error) {
	targets := make([]Target, 0, len(agent.Spec.Tools.MCPServers))
	for _, ref := range agent.Spec.Tools.MCPServers {
		raw, transport, where := ref.URL, agentoryv1alpha1.MCPTransportStreamableHTTP, "url"
		if raw == "" {
			server, found, err := m.lookup(ctx, agent.Namespace, ref.Name)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				return nil, &ToolsStatus{Reason: ReasonMCPServerNotFound, Message: fmt.Sprintf(
					"MCP server %q: no MCPServer %q in namespace %q or the registry namespace %q, and no url given",
					ref.Name, ref.Name, agent.Namespace, m.RegistryNamespace)}, nil
			}
			raw, transport, where = server.Spec.URL, server.Spec.Transport, "MCPServer "+server.Namespace+"/"+server.Name
		}
		t, err := ParseURL(ref.Name, raw, transport)
		if err != nil {
			return nil, &ToolsStatus{Reason: ReasonInvalidMCPURL, Message: err.Error() + " (from " + where + ")"}, nil
		}
		t.Tools = ref.Tools
		targets = append(targets, t)
	}
	return targets, nil, nil
}

func (m *Manager) lookup(ctx context.Context, namespace, name string) (*agentoryv1alpha1.MCPServer, bool, error) {
	for _, ns := range []string{namespace, m.RegistryNamespace} {
		if ns == "" {
			continue
		}
		var server agentoryv1alpha1.MCPServer
		switch err := m.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &server); {
		case err == nil:
			return &server, true, nil
		case !apierrors.IsNotFound(err):
			return nil, false, fmt.Errorf("getting MCPServer %s/%s: %w", ns, name, err)
		}
	}
	return nil, false, nil
}

func newObject(gvk schema.GroupVersionKind, agent *agentoryv1alpha1.Agent) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(agent.Namespace)
	u.SetName(ObjectName(agent))
	return u
}

// Ensure resolves the Agent's servers and maintains its MCP backend, route and policy.
func (m *Manager) Ensure(ctx context.Context, agent *agentoryv1alpha1.Agent) (ToolsStatus, error) {
	targets, problem, err := m.Resolve(ctx, agent)
	if err != nil {
		return ToolsStatus{}, err
	}
	if problem != nil {
		return *problem, nil
	}
	labels := agentruntime.ObjectLabels(agent)

	backend := newObject(a2a.BackendGVK, agent)
	route := newObject(a2a.HTTPRouteGVK, agent)
	policy := newObject(PolicyGVK, agent)
	for _, step := range []struct {
		obj  *unstructured.Unstructured
		spec map[string]any
	}{
		{backend, backendSpec(targets)},
		{route, m.routeSpec(agent)},
		{policy, policySpec(agent, targets)},
	} {
		obj, spec := step.obj, step.spec
		if err := ownership.Ensure(ctx, m.Client, m.Scheme, agent, obj, labels, func() {
			obj.Object["spec"] = spec
		}); err != nil {
			if errors.Is(err, ownership.ErrNotOwned) {
				return ToolsStatus{Reason: ReasonResourceConflict, Message: err.Error()}, nil
			}
			return ToolsStatus{}, err
		}
	}
	return m.routeStatus(agent, route, targets), nil
}

func backendSpec(targets []Target) map[string]any {
	ts := make([]any, 0, len(targets))
	for _, t := range targets {
		static := map[string]any{
			"host": t.Host, "port": t.Port, "path": t.Path, "protocol": string(t.Transport),
		}
		if t.TLS {
			static["policies"] = map[string]any{"tls": map[string]any{"sni": t.Host}}
		}
		ts = append(ts, map[string]any{fieldName: t.Name, "static": static})
	}
	// Always prefix tool names with the server name, so names stay stable when
	// servers are added and never collide across servers.
	return map[string]any{"mcp": map[string]any{"prefixMode": "Always", "targets": ts}}
}

func (m *Manager) routeSpec(agent *agentoryv1alpha1.Agent) map[string]any {
	return map[string]any{
		"parentRefs": []any{map[string]any{
			fieldName: m.Config.AgentGateway.Name, "namespace": m.Config.AgentGateway.Namespace,
		}},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": Path(agent)}}},
			"backendRefs": []any{map[string]any{
				"group": a2a.BackendGVK.Group, "kind": a2a.BackendGVK.Kind, fieldName: ObjectName(agent),
			}},
		}},
	}
}

func policySpec(agent *agentoryv1alpha1.Agent, targets []Target) map[string]any {
	return map[string]any{
		"targetRefs": []any{map[string]any{
			"group": a2a.BackendGVK.Group, "kind": a2a.BackendGVK.Kind, fieldName: ObjectName(agent),
		}},
		"backend": map[string]any{"mcp": map[string]any{"authorization": map[string]any{
			"action": "Allow",
			"policy": map[string]any{"matchExpressions": AuthorizationExpressions(targets)},
		}}},
	}
}

// routeStatus reads the gateway's verdict on the tool route.
func (m *Manager) routeStatus(agent *agentoryv1alpha1.Agent, route *unstructured.Unstructured, targets []Target) ToolsStatus {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	for _, p := range parents {
		parent, _ := p.(map[string]any)
		if name, _, _ := unstructured.NestedString(parent, "parentRef", fieldName); name != m.Config.AgentGateway.Name {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(parent, "conditions")
		accepted, resolved := conditionStatus(conds, "Accepted"), conditionStatus(conds, "ResolvedRefs")
		switch {
		case accepted == "True" && resolved == "True":
			return ToolsStatus{Ready: true, Reason: ReasonToolsRouted, Message: fmt.Sprintf(
				"MCP servers %s routed at %s (tool names are <server>_<tool>)", strings.Join(names, ", "), Path(agent))}
		case accepted == "False" || resolved == "False":
			return ToolsStatus{Reason: ReasonToolsRejected, Message: "gateway rejected the MCP route"}
		}
	}
	return ToolsStatus{Reason: ReasonToolsPending, Message: "waiting for the gateway to accept the MCP route"}
}

func conditionStatus(conds []any, conditionType string) string {
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == conditionType {
			s, _ := m["status"].(string)
			return s
		}
	}
	return ""
}

// Remove deletes the Agent's MCP objects if it owns them (tools were removed).
func (m *Manager) Remove(ctx context.Context, agent *agentoryv1alpha1.Agent) error {
	for _, gvk := range []schema.GroupVersionKind{a2a.HTTPRouteGVK, PolicyGVK, a2a.BackendGVK} {
		if err := ownership.Delete(ctx, m.Client, agent, newObject(gvk, agent)); err != nil {
			return err
		}
	}
	return nil
}
