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
})
