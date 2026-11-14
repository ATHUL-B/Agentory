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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// TaskReconciler reconciles a Task object.
//
// Scope today (backlog item 3): resolve the referenced Agent and record its
// revision. Starting work (JobBackend / A2A dispatch) and timeouts arrive with
// the runtime backends in backlog items 4–8.
type TaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentory.dev,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentory.dev,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentory.dev,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentory.dev,resources=agents,verbs=get;list;watch

// Reconcile brings a Task's status in line with its spec and its Agent.
func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var task agentoryv1alpha1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if task.Status.Phase.IsTerminal() {
		return ctrl.Result{}, nil
	}

	agent, err := r.lookupAgent(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}

	desired := r.desiredStatus(&task, agent)
	if equality.Semantic.DeepEqual(desired, &task.Status) {
		return ctrl.Result{}, nil
	}
	task.Status = *desired
	if err := r.Status().Update(ctx, &task); err != nil {
		return statusUpdateResult(err)
	}
	log.Info("task status updated", "phase", desired.Phase, "agentRevision", desired.AgentRevision)
	return ctrl.Result{}, nil
}

// lookupAgent returns the referenced Agent, or nil if it does not exist.
func (r *TaskReconciler) lookupAgent(ctx context.Context, task *agentoryv1alpha1.Task) (*agentoryv1alpha1.Agent, error) {
	var agent agentoryv1alpha1.Agent
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := r.Get(ctx, key, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting agent %s: %w", key, err)
	}
	return &agent, nil
}

func (r *TaskReconciler) desiredStatus(task *agentoryv1alpha1.Task, agent *agentoryv1alpha1.Agent) *agentoryv1alpha1.TaskStatus {
	status := task.Status.DeepCopy()
	status.ObservedGeneration = task.Generation
	if status.Phase == "" {
		status.Phase = agentoryv1alpha1.TaskPhasePending
	}

	if agent == nil {
		status.AgentRevision = ""
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               ConditionAgentResolved,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonAgentNotFound,
			Message:            fmt.Sprintf("agent %q not found in namespace %q", task.Spec.AgentRef.Name, task.Namespace),
			ObservedGeneration: task.Generation,
		})
	} else {
		status.AgentRevision = agent.Status.Revision
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               ConditionAgentResolved,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAgentFound,
			Message:            fmt.Sprintf("agent %q found", agent.Name),
			ObservedGeneration: task.Generation,
		})
	}

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               ConditionStarted,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonBackendNotImplemented,
		Message:            "task execution arrives with the runtime backends (backlog items 4-8)",
		ObservedGeneration: task.Generation,
	})
	return status
}

// tasksForAgent maps an Agent event to the non-terminal Tasks that reference it,
// so Tasks waiting on a missing or changed Agent are reconciled again.
func (r *TaskReconciler) tasksForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	var tasks agentoryv1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "listing tasks for agent", "agent", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for _, task := range tasks.Items {
		if task.Spec.AgentRef.Name == obj.GetName() && !task.Status.Phase.IsTerminal() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: task.Namespace, Name: task.Name},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentoryv1alpha1.Task{}).
		Watches(&agentoryv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.tasksForAgent)).
		Named("task").
		Complete(r)
}
