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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
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
		Expect(started.Reason).To(Equal(ReasonDispatchNotImplemented))
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

	Context("with a harness agent", func() {
		BeforeEach(func() {
			backends := agentruntime.NewRegistry(agentruntime.NewJobBackend(k8sClient, k8sClient.Scheme(),
				agentruntime.DefaultGatewayConfig(), map[agentoryv1alpha1.HarnessKind]string{agentoryv1alpha1.HarnessKindClaudeCode: "harness:test"}))
			taskReconciler.Backends = backends
			agentReconciler.Backends = backends
		})

		harnessAgent := func(name string) *agentoryv1alpha1.Agent {
			return newDeclarativeAgent(name, func(a *agentoryv1alpha1.Agent) {
				a.Spec.Runtime = agentoryv1alpha1.RuntimeHarness
				a.Spec.Lifecycle = agentoryv1alpha1.LifecyclePerTask
				a.Spec.Declarative = nil
				a.Spec.Harness = &agentoryv1alpha1.HarnessSpec{Kind: agentoryv1alpha1.HarnessKindClaudeCode}
			})
		}

		It("waits for the agent, runs one Job and records the harness result", func() {
			agent := harnessAgent(uniqueName("coder"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)
			task := newTask(uniqueName("fix"), agent.Name)
			Expect(k8sClient.Create(ctx, task)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, task)

			By("not starting before the agent is Ready")
			got := reconcileTask(task)
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionStarted).Reason).To(Equal(ReasonAgentNotReady))
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(testNamespace),
				client.MatchingLabels{agentruntime.LabelTask: task.Name})).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())

			By("starting once the agent is Ready")
			readyAgent := createReconciledAgentFrom(agentReconciler, agent)
			Expect(meta.IsStatusConditionTrue(readyAgent.Status.Conditions, ConditionReady)).To(BeTrue())
			got = reconcileTask(task)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionStarted)).To(BeTrue())
			Expect(got.Status.AgentRevision).To(Equal(readyAgent.Status.Revision))
			var job batchv1.Job
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: task.Name}, &job)).To(Succeed())
			Expect(metav1.IsControlledBy(&job, got)).To(BeTrue())
			Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("harness:test"))

			By("finishing with the harness's result")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: task.Name + "-x1", Namespace: testNamespace,
					Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{Name: "harness", Image: "harness:test"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, pod)
			pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "harness", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Message: `{"status":"succeeded","summary":"Created hello.txt","traceId":"0af7651916cd43dd8448eb211c80319c",` +
						`"usage":{"inputTokens":75,"outputTokens":15,"costUsd":"0.000750"},` +
						`"artifacts":[{"type":"file","ref":"hello.txt"}]}`}}}}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			now := metav1.Now()
			job.Status = batchv1.JobStatus{StartTime: &now, CompletionTime: &now, Succeeded: 1, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now}}}
			Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

			got = reconcileTask(task)
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.TaskPhaseSucceeded))
			Expect(got.Status.TraceID).To(Equal("0af7651916cd43dd8448eb211c80319c"))
			Expect(got.Status.Usage).To(Equal(&agentoryv1alpha1.TaskUsage{InputTokens: 75, OutputTokens: 15, CostUSD: "0.000750"}))
			Expect(got.Status.Result.Summary).To(Equal("Created hello.txt"))
			Expect(got.Status.Result.Artifacts).To(ConsistOf(agentoryv1alpha1.TaskArtifact{Type: "file", Ref: "hello.txt"}))
			Expect(got.Status.StartedAt).NotTo(BeNil())
			Expect(got.Status.CompletedAt).NotTo(BeNil())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionComplete)).To(BeTrue())

			By("staying put once terminal")
			again := reconcileTask(task)
			Expect(again.ResourceVersion).To(Equal(got.ResourceVersion))
		})
	})
})

// createReconciledAgentFrom reconciles an existing Agent and returns it.
func createReconciledAgentFrom(r *AgentReconciler, agent *agentoryv1alpha1.Agent) *agentoryv1alpha1.Agent {
	key := client.ObjectKeyFromObject(agent)
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	Expect(err).NotTo(HaveOccurred())
	var got agentoryv1alpha1.Agent
	Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
	return &got
}
