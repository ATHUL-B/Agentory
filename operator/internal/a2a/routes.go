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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/ownership"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

// Kinds the route manager creates. Unstructured, so the operator does not
// depend on the Gateway API or agentgateway Go modules.
var (
	HTTPRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	BackendGVK   = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayBackend"}
)

// fieldType is the discriminator field name used throughout Gateway API objects.
const fieldType = "type"

// Condition reasons for GatewayRouteReady.
const (
	ReasonRouteAccepted    = "RouteAccepted"
	ReasonRoutePending     = "RoutePending"
	ReasonRouteRejected    = "RouteRejected"
	ReasonResourceConflict = "ResourceConflict"
)

// RouteStatus reports an Agent's gateway route.
type RouteStatus struct {
	Ready   bool
	Reason  string
	Message string
}

// Exposed reports whether the Agent should be reachable over A2A
// (spec.a2a.expose, default true).
func Exposed(agent *agentoryv1alpha1.Agent) bool {
	return agent.Spec.A2A == nil || agent.Spec.A2A.Expose == nil || *agent.Spec.A2A.Expose
}

// RouteManager maintains an Agent's card, A2A backend and HTTPRoute.
type RouteManager struct {
	Client client.Client
	Scheme *k8sruntime.Scheme
	Config agentruntime.GatewayConfig
	Cards  CardBuilder
}

// NewRouteManager returns a manager that attaches routes to the Gateway that
// backs cfg.AgentGateway (agentgateway names the Service after the Gateway).
func NewRouteManager(c client.Client, scheme *k8sruntime.Scheme, cfg agentruntime.GatewayConfig) *RouteManager {
	return &RouteManager{Client: c, Scheme: scheme, Config: cfg, Cards: CardBuilder{Config: cfg}}
}

func newObject(gvk schema.GroupVersionKind, agent *agentoryv1alpha1.Agent) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(agent.Namespace)
	u.SetName(agent.Name)
	return u
}

// EnsureCard writes the Agent's card to the ConfigMap its workload mounts.
func (m *RouteManager) EnsureCard(ctx context.Context, agent *agentoryv1alpha1.Agent, revision string) error {
	data, err := json.MarshalIndent(m.Cards.Build(agent, revision), "", "  ")
	if err != nil {
		return fmt.Errorf("encoding agent card: %w", err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: agentruntime.AgentCardConfigMapName(agent), Namespace: agent.Namespace,
	}}
	return ownership.Ensure(ctx, m.Client, m.Scheme, agent, cm, agentruntime.ObjectLabels(agent), func() {
		cm.Data = map[string]string{agentruntime.AgentCardFile: string(data)}
	})
}

// EnsureRoute creates the A2A backend and HTTPRoute for a workload serving on
// port, and reports whether the gateway accepted the route.
func (m *RouteManager) EnsureRoute(ctx context.Context, agent *agentoryv1alpha1.Agent, port int32) (RouteStatus, error) {
	labels := agentruntime.ObjectLabels(agent)
	host := fmt.Sprintf("%s.%s.svc.%s", agent.Name, agent.Namespace, m.Config.ClusterDomain)

	backend := newObject(BackendGVK, agent)
	if err := ownership.Ensure(ctx, m.Client, m.Scheme, agent, backend, labels, func() {
		backend.Object["spec"] = map[string]any{"a2a": map[string]any{"host": host, "port": int64(port)}}
	}); err != nil {
		return conflictOr(err)
	}

	route := newObject(HTTPRouteGVK, agent)
	if err := ownership.Ensure(ctx, m.Client, m.Scheme, agent, route, labels, func() {
		route.Object["spec"] = m.routeSpec(agent)
	}); err != nil {
		return conflictOr(err)
	}
	return m.routeStatus(route), nil
}

func (m *RouteManager) routeSpec(agent *agentoryv1alpha1.Agent) map[string]any {
	return map[string]any{
		"parentRefs": []any{map[string]any{
			"name": m.Config.AgentGateway.Name, "namespace": m.Config.AgentGateway.Namespace,
		}},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{"path": map[string]any{fieldType: "PathPrefix", "value": Path(agent)}}},
			// The agent serves at "/", so strip the gateway prefix.
			"filters": []any{map[string]any{
				fieldType: "URLRewrite",
				"urlRewrite": map[string]any{"path": map[string]any{
					fieldType: "ReplacePrefixMatch", "replacePrefixMatch": "/",
				}},
			}},
			"backendRefs": []any{map[string]any{
				"group": BackendGVK.Group, "kind": BackendGVK.Kind, "name": agent.Name,
			}},
		}},
	}
}

// routeStatus reads the gateway's verdict from the HTTPRoute status for our parent.
func (m *RouteManager) routeStatus(route *unstructured.Unstructured) RouteStatus {
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	for _, p := range parents {
		parent, _ := p.(map[string]any)
		name, _, _ := unstructured.NestedString(parent, "parentRef", "name")
		if name != m.Config.AgentGateway.Name {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(parent, "conditions")
		accepted, resolved := conditionState(conds, "Accepted"), conditionState(conds, "ResolvedRefs")
		switch {
		case accepted.status == "True" && resolved.status == "True":
			return RouteStatus{Ready: true, Reason: ReasonRouteAccepted,
				Message: fmt.Sprintf("route %s accepted by gateway %s", route.GetName(), name)}
		case accepted.status == "False":
			return RouteStatus{Reason: ReasonRouteRejected, Message: "gateway rejected the route: " + accepted.message}
		case resolved.status == "False":
			return RouteStatus{Reason: ReasonRouteRejected, Message: "gateway could not resolve the backend: " + resolved.message}
		}
	}
	return RouteStatus{Reason: ReasonRoutePending, Message: "waiting for the gateway to accept the route"}
}

type condition struct{ status, message string }

func conditionState(conds []any, conditionType string) condition {
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m[fieldType] == conditionType {
			s, _ := m["status"].(string)
			msg, _ := m["message"].(string)
			return condition{s, msg}
		}
	}
	return condition{}
}

// Remove deletes the Agent's route, backend and card if the Agent owns them
// (used when exposure is turned off). Deleting the Agent removes them by
// garbage collection.
func (m *RouteManager) Remove(ctx context.Context, agent *agentoryv1alpha1.Agent) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: agentruntime.AgentCardConfigMapName(agent), Namespace: agent.Namespace,
	}}
	for _, obj := range []client.Object{newObject(HTTPRouteGVK, agent), newObject(BackendGVK, agent), cm} {
		if err := ownership.Delete(ctx, m.Client, agent, obj); err != nil {
			return err
		}
	}
	return nil
}

func conflictOr(err error) (RouteStatus, error) {
	if errors.Is(err, ownership.ErrNotOwned) {
		return RouteStatus{Reason: ReasonResourceConflict, Message: err.Error()}, nil
	}
	return RouteStatus{}, err
}
