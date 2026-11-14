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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

var _ = Describe("Task controller", func() {
	var (
		taskReconciler  *TaskReconciler
		agentReconciler *AgentReconciler
	)

	BeforeEach(func() {
		taskReconciler = &TaskReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		agentReconciler = &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	reconcileTask := func(task *agentoryv1alpha1.Task) *agentoryv1alpha1.Task {
		key := client.ObjectKeyFromObject(task)
		_, err := taskReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var got agentoryv1alpha1.Task
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		return &got
	}

	createReconciledAgent := func(name string) *agentoryv1alpha1.Agent {
		agent := newDeclarativeAgent(name)
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, agent)
		key := client.ObjectKeyFromObject(agent)
		_, err := agentReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var got agentoryv1alpha1.Agent
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		return &got
	}

	It("reports a missing agent", func() {
		task := newTask(uniqueName("task"), "no-such-agent")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, task)

		got := reconcileTask(task)

		Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.TaskPhasePending))
		resolved := meta.FindStatusCondition(got.Status.Conditions, ConditionAgentResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
		Expect(resolved.Reason).To(Equal(ReasonAgentNotFound))
		Expect(got.Status.AgentRevision).To(BeEmpty())
	})

	It("resolves the agent and records its revision once it exists", func() {
		agentName := uniqueName("agent")
		task := newTask(uniqueName("task"), agentName)
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, task)
		Expect(reconcileTask(task).Status.AgentRevision).To(BeEmpty())

		agent := createReconciledAgent(agentName)
		got := reconcileTask(task)

		resolved := meta.FindStatusCondition(got.Status.Conditions, ConditionAgentResolved)
		Expect(resolved.Status).To(Equal(metav1.ConditionTrue))
		Expect(resolved.Reason).To(Equal(ReasonAgentFound))
		Expect(got.Status.AgentRevision).To(Equal(agent.Status.Revision))

		started := meta.FindStatusCondition(got.Status.Conditions, ConditionStarted)
		Expect(started.Status).To(Equal(metav1.ConditionFalse))
		Expect(started.Reason).To(Equal(ReasonBackendNotImplemented))
	})

	It("leaves terminal tasks untouched", func() {
		task := newTask(uniqueName("task"), "no-such-agent")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, task)
		task.Status.Phase = agentoryv1alpha1.TaskPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

		got := reconcileTask(task)
		Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.TaskPhaseSucceeded))
		Expect(got.Status.Conditions).To(BeEmpty())
	})

	It("maps an agent event to the non-terminal tasks that reference it", func() {
		agentName := uniqueName("agent")
		waiting := newTask(uniqueName("task"), agentName)
		done := newTask(uniqueName("task"), agentName)
		other := newTask(uniqueName("task"), "some-other-agent")
		for _, t := range []*agentoryv1alpha1.Task{waiting, done, other} {
			Expect(k8sClient.Create(ctx, t)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, t)
		}
		done.Status.Phase = agentoryv1alpha1.TaskPhaseFailed
		Expect(k8sClient.Status().Update(ctx, done)).To(Succeed())

		requests := taskReconciler.tasksForAgent(ctx, newDeclarativeAgent(agentName))

		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: waiting.Name},
		}))
	})
})
