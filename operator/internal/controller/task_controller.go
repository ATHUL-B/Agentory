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

	batchv1 "k8s.io/api/batch/v1"
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
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

// Task condition reasons.
const (
	ReasonAgentNotReady          = "AgentNotReady"
	ReasonDispatchNotImplemented = "DispatchNotImplemented"
	ReasonWorkloadCreated        = "WorkloadCreated"
)

// TaskReconciler reconciles a Task: once its Agent is Ready, it hands the Task
// to the Agent's task backend (one Job per Task for harness agents) and copies
// the workload's progress and result into the Task's status.
type TaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Backends selects the runtime backend per Agent. Nil means none.
	Backends *agentruntime.Registry
}

// +kubebuilder:rbac:groups=agentory.dev,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentory.dev,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentory.dev,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentory.dev,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list

// Reconcile brings a Task's status in line with its spec, its Agent and its workload.
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

	desired, err := r.desiredStatus(ctx, &task, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(desired, &task.Status) {
		return ctrl.Result{}, nil
	}
	task.Status = *desired
	if err := r.Status().Update(ctx, &task); err != nil {
		return statusUpdateResult(err)
	}
	log.Info("Updated Task status", "phase", desired.Phase, "agentRevision", desired.AgentRevision)
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

func (r *TaskReconciler) desiredStatus(ctx context.Context, task *agentoryv1alpha1.Task,
	agent *agentoryv1alpha1.Agent) (*agentoryv1alpha1.TaskStatus, error) {
	status := task.Status.DeepCopy()
	status.ObservedGeneration = task.Generation
	if status.Phase == "" {
		status.Phase = agentoryv1alpha1.TaskPhasePending
	}
	started := meta.IsStatusConditionTrue(status.Conditions, ConditionStarted)

	if agent == nil {
		if !started {
			status.AgentRevision = ""
		}
		r.setCondition(status, task, ConditionAgentResolved, false, ReasonAgentNotFound,
			fmt.Sprintf("agent %q not found in namespace %q", task.Spec.AgentRef.Name, task.Namespace))
		if !started {
			r.setCondition(status, task, ConditionStarted, false, ReasonAgentNotFound, "waiting for the agent to exist")
		}
		return status, nil
	}
	r.setCondition(status, task, ConditionAgentResolved, true, ReasonAgentFound, fmt.Sprintf("agent %q found", agent.Name))
	// A Task records the revision it starts against; once started, that never changes.
	if !started {
		status.AgentRevision = agent.Status.Revision
	}

	backend, ok := r.Backends.TaskBackendFor(agent)
	if !ok {
		r.setCondition(status, task, ConditionStarted, false, ReasonDispatchNotImplemented,
			fmt.Sprintf("no task runner for runtime=%s lifecycle=%s yet; service agents take work over A2A",
				agent.Spec.Runtime, agent.Spec.Lifecycle))
		return status, nil
	}
	if !started && !meta.IsStatusConditionTrue(agent.Status.Conditions, ConditionReady) {
		msg := "waiting for the agent to be Ready"
		if c := meta.FindStatusCondition(agent.Status.Conditions, ConditionReady); c != nil && c.Message != "" {
			msg += ": " + c.Message
		}
		r.setCondition(status, task, ConditionStarted, false, ReasonAgentNotReady, msg)
		return status, nil
	}

	rt, err := backend.EnsureTask(ctx, agent, task, agentruntime.Deps{Revision: status.AgentRevision})
	if err != nil {
		return nil, fmt.Errorf("starting task: %w", err)
	}
	r.applyRuntimeStatus(status, task, rt)
	return status, nil
}

// applyRuntimeStatus copies a workload's progress and result into the Task status.
func (r *TaskReconciler) applyRuntimeStatus(status *agentoryv1alpha1.TaskStatus, task *agentoryv1alpha1.Task,
	rt agentruntime.TaskRuntimeStatus) {
	if rt.Reason == agentruntime.ReasonResourceConflict {
		status.Phase = agentoryv1alpha1.TaskPhaseFailed
		r.setCondition(status, task, ConditionStarted, false, rt.Reason, rt.Message)
		return
	}
	r.setCondition(status, task, ConditionStarted, true, ReasonWorkloadCreated, "workload "+rt.Workload)
	status.Phase = rt.Phase
	status.StartedAt = rt.StartedAt
	status.CompletedAt = rt.CompletedAt

	if res := rt.Result; res != nil {
		status.TraceID = res.TraceID
		status.Usage = &agentoryv1alpha1.TaskUsage{
			InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens, CostUSD: res.Usage.CostUSD,
		}
		status.Result = &agentoryv1alpha1.TaskResult{Summary: res.Summary, Artifacts: res.Artifacts}
	}
	if rt.Phase.IsTerminal() {
		r.setCondition(status, task, ConditionComplete, rt.Phase == agentoryv1alpha1.TaskPhaseSucceeded, rt.Reason, rt.Message)
	} else if rt.Reason == agentruntime.ReasonImageProblem {
		r.setCondition(status, task, ConditionComplete, false, rt.Reason, rt.Message)
	} else {
		meta.RemoveStatusCondition(&status.Conditions, ConditionComplete)
	}
}

func (r *TaskReconciler) setCondition(status *agentoryv1alpha1.TaskStatus, task *agentoryv1alpha1.Task,
	conditionType string, ok bool, reason, message string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: s, Reason: reason, Message: message, ObservedGeneration: task.Generation,
	})
}

// tasksForAgent maps an Agent event to the non-terminal Tasks that reference it,
// so Tasks waiting on a missing or not-yet-Ready Agent are reconciled again.
func (r *TaskReconciler) tasksForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	var tasks agentoryv1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Tasks for Agent", "agent", obj.GetName())
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
		Owns(&batchv1.Job{}).
		Watches(&agentoryv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.tasksForAgent)).
		Named("task").
		Complete(r)
}
