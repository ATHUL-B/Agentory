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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// AgentReconciler reconciles an Agent object.
//
// Scope today (backlog item 3): record the spec revision and acceptance in the
// status. Runtime backends that create workloads, gateway routes and virtual
// keys are added in backlog items 4–8, behind the RuntimeBackend interface
// (HLD-0001 §4).
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentory.dev,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentory.dev,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentory.dev,resources=agents/finalizers,verbs=update

// Reconcile brings an Agent's status in line with its spec.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent agentoryv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desired, err := r.desiredStatus(&agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(desired, &agent.Status) {
		return ctrl.Result{}, nil
	}

	agent.Status = *desired
	if err := r.Status().Update(ctx, &agent); err != nil {
		// Conflicts are expected when the object changed meanwhile; the next event retries.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("agent status updated", "revision", desired.Revision, "phase", desired.Phase)
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) desiredStatus(agent *agentoryv1alpha1.Agent) (*agentoryv1alpha1.AgentStatus, error) {
	revision, err := SpecRevision(agent.Name, &agent.Spec)
	if err != nil {
		return nil, err
	}

	status := agent.Status.DeepCopy()
	status.ObservedGeneration = agent.Generation
	status.Revision = revision
	status.Phase = agentoryv1alpha1.AgentPhasePending

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               ConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonSpecValid,
		Message:            "spec passed validation",
		ObservedGeneration: agent.Generation,
	})
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:   ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: ReasonBackendNotImplemented,
		Message: fmt.Sprintf("no runtime backend yet for runtime=%s lifecycle=%s (backlog items 4-8)",
			agent.Spec.Runtime, agent.Spec.Lifecycle),
		ObservedGeneration: agent.Generation,
	})
	return status, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentoryv1alpha1.Agent{}).
		Named("agent").
		Complete(r)
}
