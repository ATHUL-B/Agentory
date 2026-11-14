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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

var _ = Describe("Agent controller", func() {
	var reconciler *AgentReconciler

	BeforeEach(func() {
		reconciler = &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	reconcileAgent := func(agent *agentoryv1alpha1.Agent) *agentoryv1alpha1.Agent {
		key := client.ObjectKeyFromObject(agent)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var got agentoryv1alpha1.Agent
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		return &got
	}

	It("records revision, generation and acceptance", func() {
		agent := newDeclarativeAgent(uniqueName("agent"))
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, agent)

		got := reconcileAgent(agent)

		wantRevision, err := SpecRevision(agent.Name, &got.Spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.Revision).To(Equal(wantRevision))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhasePending))

		accepted := meta.FindStatusCondition(got.Status.Conditions, ConditionAccepted)
		Expect(accepted).NotTo(BeNil())
		Expect(accepted.Status).To(Equal(metav1.ConditionTrue))
		Expect(accepted.Reason).To(Equal(ReasonSpecValid))

		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonBackendNotImplemented))
		Expect(ready.Message).To(ContainSubstring("runtime=declarative lifecycle=service"))
	})

	It("is idempotent: a second reconcile changes nothing", func() {
		agent := newDeclarativeAgent(uniqueName("agent"))
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, agent)

		first := reconcileAgent(agent)
		second := reconcileAgent(agent)
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
	})

	It("produces a new revision when the spec changes", func() {
		agent := newDeclarativeAgent(uniqueName("agent"))
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, agent)
		before := reconcileAgent(agent)

		before.Spec.Declarative.Instructions = "You are a stricter on-call agent."
		Expect(k8sClient.Update(ctx, before)).To(Succeed())
		after := reconcileAgent(before)

		Expect(after.Status.Revision).NotTo(Equal(before.Status.Revision))
		Expect(after.Status.ObservedGeneration).To(Equal(after.Generation))
		Expect(after.Generation).To(BeNumerically(">", before.Status.ObservedGeneration))
	})

	It("ignores agents that no longer exist", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "does-not-exist"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with the deployment backend", func() {
		BeforeEach(func() {
			reconciler.Backends = agentruntime.NewRegistry(
				agentruntime.NewDeploymentBackend(k8sClient, k8sClient.Scheme(), agentruntime.DefaultGatewayConfig()))
		})

		containerAgent := func(name string) *agentoryv1alpha1.Agent {
			return newDeclarativeAgent(name, func(a *agentoryv1alpha1.Agent) {
				a.Spec.Runtime = agentoryv1alpha1.RuntimeContainer
				a.Spec.Declarative = nil
				a.Spec.Container = &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/crew:1.0.0"}
			})
		}

		It("creates the workload and becomes Ready once the deployment is available", func() {
			agent := containerAgent(uniqueName("crew"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			got := reconcileAgent(agent)
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhasePending))
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionEgressPolicyApplied)).To(BeTrue())
			workload := meta.FindStatusCondition(got.Status.Conditions, ConditionWorkloadReady)
			Expect(workload.Status).To(Equal(metav1.ConditionFalse))
			Expect(workload.Reason).To(Equal(agentruntime.ReasonProgressing))

			key := client.ObjectKeyFromObject(agent)
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
				Expect(k8sClient.Get(ctx, key, obj)).To(Succeed(), "%T", obj)
				Expect(metav1.IsControlledBy(obj, got)).To(BeTrue(), "%T must be owned by the agent", obj)
			}

			// envtest runs no deployment controller: report availability ourselves.
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, key, &deploy)).To(Succeed())
			deploy.Status = appsv1.DeploymentStatus{
				ObservedGeneration: deploy.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			}
			Expect(k8sClient.Status().Update(ctx, &deploy)).To(Succeed())

			got = reconcileAgent(agent)
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhaseReady))
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue())
			Expect(got.Status.Endpoints).NotTo(BeNil())
			Expect(got.Status.Endpoints.Internal).To(Equal(
				"http://" + agent.Name + "." + testNamespace + ".svc.cluster.local:8080"))
		})

		It("fails without touching a foreign object that has the agent's name", func() {
			name := uniqueName("taken")
			foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, foreign)
			agent := containerAgent(name)
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			got := reconcileAgent(agent)
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhaseFailed))
			ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			Expect(ready.Reason).To(Equal(agentruntime.ReasonResourceConflict))
			Expect(ready.Message).To(ContainSubstring("is not managed by this Agent"))

			var sa corev1.ServiceAccount
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), &sa)).To(Succeed())
			Expect(sa.OwnerReferences).To(BeEmpty())
		})

		It("leaves agents without a backend at BackendNotImplemented", func() {
			agent := newDeclarativeAgent(uniqueName("decl"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			ready := meta.FindStatusCondition(reconcileAgent(agent).Status.Conditions, ConditionReady)
			Expect(ready.Reason).To(Equal(ReasonBackendNotImplemented))
		})
	})
})
