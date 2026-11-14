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

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/a2a"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
	"github.com/ATHUL-B/Agentory/operator/internal/virtualkey"
)

// FinalizerVirtualKey makes the operator delete an Agent's gateway key before
// the Agent goes away (the key lives outside the cluster, so garbage collection
// can't remove it).
const FinalizerVirtualKey = "agentory.dev/virtual-key"

// virtualKeyResync re-checks keys periodically, so a key the gateway lost (for
// example after its database was reset) is re-created without waiting for an event.
const virtualKeyResync = 10 * time.Minute

// missingSkillRetry is how often an Agent re-checks for a skill ConfigMap that doesn't exist yet.
const missingSkillRetry = 15 * time.Second

// AgentReconciler reconciles an Agent object: it records the spec revision and
// acceptance, ensures the Agent's LLM gateway virtual key, then hands the Agent
// to the runtime backend that supports it (HLD-0001 §4). Agents no backend
// supports yet stay Ready=False with reason BackendNotImplemented.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Backends selects the runtime backend per Agent. Nil means none.
	Backends *agentruntime.Registry
	// VirtualKeys manages per-agent gateway keys. Nil disables key management.
	VirtualKeys *virtualkey.Manager
	// Routes exposes Agents over A2A on the agent gateway. Nil disables exposure.
	Routes *a2a.RouteManager
}

// +kubebuilder:rbac:groups=agentory.dev,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentory.dev,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentory.dev,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentgateway.dev,resources=agentgatewaybackends,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings an Agent's status in line with its spec.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent agentoryv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !agent.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &agent)
	}
	if r.VirtualKeys != nil && controllerutil.AddFinalizer(&agent, FinalizerVirtualKey) {
		// The update triggers the next reconcile.
		if err := r.Update(ctx, &agent); err != nil {
			return statusUpdateResult(err)
		}
		return ctrl.Result{}, nil
	}

	desired, retryErr, err := r.desiredStatus(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if r.VirtualKeys != nil {
		result.RequeueAfter = virtualKeyResync
	}
	// ConfigMaps aren't watched (that would cache every one in the cluster), so
	// poll while a skill is missing; it usually shows up moments later.
	if c := meta.FindStatusCondition(desired.Conditions, ConditionReady); c != nil && c.Reason == agentruntime.ReasonSkillNotFound {
		result.RequeueAfter = missingSkillRetry
	}

	if !equality.Semantic.DeepEqual(desired, &agent.Status) {
		agent.Status = *desired
		if err := r.Status().Update(ctx, &agent); err != nil {
			return statusUpdateResult(err)
		}
		log.Info("Updated Agent status", "revision", desired.Revision, "phase", desired.Phase)
	}
	// A transient failure (e.g. gateway unreachable) is reported in status first, then retried with backoff.
	return result, retryErr
}

// finalize removes what lives outside the cluster, then releases the Agent.
func (r *AgentReconciler) finalize(ctx context.Context, agent *agentoryv1alpha1.Agent) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(agent, FinalizerVirtualKey) {
		return ctrl.Result{}, nil
	}
	if r.VirtualKeys != nil {
		if err := r.VirtualKeys.Cleanup(ctx, agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting virtual key: %w", err)
		}
		logf.FromContext(ctx).Info("Deleted virtual key", "key", virtualkey.KeyName(agent))
	} else {
		logf.FromContext(ctx).Info("Released Agent without deleting its virtual key; key management is disabled",
			"key", virtualkey.KeyName(agent))
	}
	controllerutil.RemoveFinalizer(agent, FinalizerVirtualKey)
	if err := r.Update(ctx, agent); err != nil {
		return statusUpdateResult(err)
	}
	return ctrl.Result{}, nil
}

// desiredStatus computes the status. retryErr is a transient failure to report
// and retry; err aborts the reconcile without a status write.
func (r *AgentReconciler) desiredStatus(ctx context.Context, agent *agentoryv1alpha1.Agent) (
	status *agentoryv1alpha1.AgentStatus, retryErr, err error) {
	revision, err := SpecRevision(agent.Name, &agent.Spec)
	if err != nil {
		return nil, nil, err
	}

	status = agent.Status.DeepCopy()
	status.ObservedGeneration = agent.Generation
	status.Revision = revision
	status.Phase = agentoryv1alpha1.AgentPhasePending
	r.setCondition(status, agent, ConditionAccepted, true, ReasonSpecValid, "spec passed validation")

	deps := agentruntime.Deps{Revision: revision}
	keyReady := true
	if r.VirtualKeys != nil {
		vk, vkErr := r.VirtualKeys.Ensure(ctx, agent)
		keyReady = vk.Ready
		deps.VirtualKeyID = vk.KeyID
		r.setCondition(status, agent, ConditionVirtualKeyReady, vk.Ready, vk.Reason, vk.Message)
		if vk.Ready {
			status.VirtualKeyRef = &corev1.LocalObjectReference{Name: agentruntime.VirtualKeySecretName(agent)}
		}
		switch {
		case vkErr != nil:
			r.setCondition(status, agent, ConditionReady, false, vk.Reason, "virtual key: "+vk.Message)
			return status, vkErr, nil
		case vk.Reason == virtualkey.ReasonResourceConflict:
			status.Phase = agentoryv1alpha1.AgentPhaseFailed
			r.setCondition(status, agent, ConditionReady, false, vk.Reason, vk.Message)
			return status, nil, nil
		}
	} else {
		meta.RemoveStatusCondition(&status.Conditions, ConditionVirtualKeyReady)
		status.VirtualKeyRef = nil
	}

	if r.Routes != nil && a2a.Exposed(agent) && agent.Spec.Lifecycle == agentoryv1alpha1.LifecycleService {
		if err := r.Routes.EnsureCard(ctx, agent, revision); err != nil {
			return nil, nil, fmt.Errorf("agent card: %w", err)
		}
	}

	backend, ok := r.Backends.For(agent)
	if !ok {
		status.Endpoints = nil
		r.setCondition(status, agent, ConditionReady, false, ReasonBackendNotImplemented,
			fmt.Sprintf("no runtime backend yet for runtime=%s lifecycle=%s (backlog items 7-8)",
				agent.Spec.Runtime, agent.Spec.Lifecycle))
		return status, nil, nil
	}

	rt, err := backend.EnsureAgent(ctx, agent, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("%s backend: %w", backend.Name(), err)
	}
	if rt.InternalEndpoint != "" {
		status.Endpoints = &agentoryv1alpha1.AgentEndpoints{Internal: rt.InternalEndpoint}
	}
	conflict := rt.Reason == agentruntime.ReasonResourceConflict
	r.setCondition(status, agent, ConditionEgressPolicyApplied, !conflict, egressReason(rt), egressMessage(rt))
	r.setCondition(status, agent, ConditionWorkloadReady, rt.WorkloadReady, rt.Reason, rt.Message)

	route, err := r.ensureRoute(ctx, agent, rt, status)
	if err != nil {
		return nil, nil, err
	}

	ready := rt.WorkloadReady && keyReady && route.Ready
	reason, message := rt.Reason, rt.Message
	if rt.WorkloadReady && !route.Ready {
		reason, message = route.Reason, route.Message
	}
	r.setCondition(status, agent, ConditionReady, ready, reason, message)
	switch {
	case ready:
		status.Phase = agentoryv1alpha1.AgentPhaseReady
	case conflict || route.Reason == a2a.ReasonResourceConflict:
		status.Phase = agentoryv1alpha1.AgentPhaseFailed
	}
	return status, nil, nil
}

// ensureRoute exposes a serving Agent on the agent gateway, or removes its
// route when it should not be exposed. The returned status is Ready when no
// route is needed.
func (r *AgentReconciler) ensureRoute(ctx context.Context, agent *agentoryv1alpha1.Agent,
	rt agentruntime.AgentRuntimeStatus, status *agentoryv1alpha1.AgentStatus) (a2a.RouteStatus, error) {
	notNeeded := a2a.RouteStatus{Ready: true}
	if r.Routes == nil {
		meta.RemoveStatusCondition(&status.Conditions, ConditionGatewayRouteReady)
		return notNeeded, nil
	}
	if !a2a.Exposed(agent) || rt.ServingPort == 0 {
		meta.RemoveStatusCondition(&status.Conditions, ConditionGatewayRouteReady)
		if status.Endpoints != nil {
			status.Endpoints.A2A, status.Endpoints.AgentCard = "", ""
		}
		return notNeeded, r.Routes.Remove(ctx, agent)
	}

	route, err := r.Routes.EnsureRoute(ctx, agent, rt.ServingPort)
	if err != nil {
		return route, fmt.Errorf("gateway route: %w", err)
	}
	if status.Endpoints == nil {
		status.Endpoints = &agentoryv1alpha1.AgentEndpoints{}
	}
	status.Endpoints.A2A = a2a.URL(r.Routes.Config, agent)
	status.Endpoints.AgentCard = status.Endpoints.A2A + a2a.CardPath
	r.setCondition(status, agent, ConditionGatewayRouteReady, route.Ready, route.Reason, route.Message)
	return route, nil
}

func (r *AgentReconciler) setCondition(status *agentoryv1alpha1.AgentStatus, agent *agentoryv1alpha1.Agent,
	conditionType string, ok bool, reason, message string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: s, Reason: reason, Message: message, ObservedGeneration: agent.Generation,
	})
}

func egressReason(rt agentruntime.AgentRuntimeStatus) string {
	if rt.Reason == agentruntime.ReasonResourceConflict {
		return rt.Reason
	}
	return ReasonPolicyApplied
}

func egressMessage(rt agentruntime.AgentRuntimeStatus) string {
	if rt.Reason == agentruntime.ReasonResourceConflict {
		return rt.Message
	}
	return "egress limited to DNS, LLM gateway, agent gateway and collector; ingress only from the agent gateway"
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&agentoryv1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{})
	if r.Routes != nil {
		// The gateway accepts routes asynchronously; watch them to see it.
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(a2a.HTTPRouteGVK)
		b = b.Owns(route)
	}
	return b.Named("agent").Complete(r)
}
