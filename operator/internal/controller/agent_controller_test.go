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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/a2a"
	gatewayfake "github.com/ATHUL-B/Agentory/operator/internal/llmgateway/fake"
	"github.com/ATHUL-B/Agentory/operator/internal/mcp"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
	"github.com/ATHUL-B/Agentory/operator/internal/virtualkey"
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

	Context("with virtual keys", func() {
		var gw *gatewayfake.Gateway

		BeforeEach(func() {
			gw = gatewayfake.New()
			reconciler.VirtualKeys = &virtualkey.Manager{Client: k8sClient, Scheme: k8sClient.Scheme(), Gateway: gw}
			reconciler.Backends = agentruntime.NewRegistry(
				agentruntime.NewDeploymentBackend(k8sClient, k8sClient.Scheme(), agentruntime.DefaultGatewayConfig()))
		})

		// reconcileUntilStable runs the reconciler until it stops changing the
		// Agent (the first pass only adds the finalizer).
		reconcileUntilStable := func(agent *agentoryv1alpha1.Agent) *agentoryv1alpha1.Agent {
			var got *agentoryv1alpha1.Agent
			for range 3 {
				got = reconcileAgent(agent)
			}
			return got
		}

		It("creates a key, stores it in the Agent's Secret and stamps the pods", func() {
			agent := newDeclarativeAgent(uniqueName("keyed"), func(a *agentoryv1alpha1.Agent) {
				a.Spec.Runtime = agentoryv1alpha1.RuntimeContainer
				a.Spec.Declarative = nil
				a.Spec.Container = &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/crew:1.0.0"}
			})
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())

			got := reconcileUntilStable(agent)
			Expect(got.Finalizers).To(ContainElement(FinalizerVirtualKey))
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionVirtualKeyReady)).To(BeTrue())
			Expect(got.Status.VirtualKeyRef).To(Equal(&corev1.LocalObjectReference{Name: agent.Name + "-llm"}))
			Expect(gw.Len()).To(Equal(1))

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: agent.Name + "-llm"}, &secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey(agentruntime.VirtualKeySecretKey))
			keyID := secret.Annotations[virtualkey.AnnotationKeyID]

			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue(agentruntime.AnnotationVirtualKeyID, keyID))

			By("deleting the Agent, the key is deleted from the gateway and the finalizer released")
			Expect(k8sClient.Delete(ctx, got)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			Expect(err).NotTo(HaveOccurred())
			Expect(gw.Len()).To(Equal(0))
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &agentoryv1alpha1.Agent{})
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred(), "agent should be gone once the finalizer is removed")
		})

		It("reports a gateway outage in status and retries", func() {
			agent := newDeclarativeAgent(uniqueName("outage"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(func() {
				gw.Err = nil
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, agent))).To(Succeed())
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			})
			reconcileAgent(agent) // adds the finalizer

			gw.Err = errors.New("connection refused")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			Expect(err).To(HaveOccurred(), "the reconcile must be retried")

			var got agentoryv1alpha1.Agent
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionVirtualKeyReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(virtualkey.ReasonGatewayError))
			Expect(cond.Message).To(ContainSubstring("connection refused"))
		})

		It("releases a deleted Agent without cleanup when key management is turned off", func() {
			agent := newDeclarativeAgent(uniqueName("orphaned"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			got := reconcileUntilStable(agent)
			Expect(got.Finalizers).To(ContainElement(FinalizerVirtualKey))

			reconciler.VirtualKeys = nil
			Expect(k8sClient.Delete(ctx, got)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			Expect(err).NotTo(HaveOccurred())
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &agentoryv1alpha1.Agent{})
			Expect(err).To(HaveOccurred(), "agent must not be stuck on the finalizer")
			Expect(gw.Len()).To(Equal(1), "the key is left for an operator with credentials to clean up")
		})
	})

	Context("with A2A exposure", func() {
		BeforeEach(func() {
			cfg := agentruntime.DefaultGatewayConfig()
			reconciler.Backends = agentruntime.NewRegistry(agentruntime.NewDeploymentBackend(k8sClient, k8sClient.Scheme(), cfg))
			reconciler.Routes = a2a.NewRouteManager(k8sClient, k8sClient.Scheme(), cfg)
		})

		exposedAgent := func(name string) *agentoryv1alpha1.Agent {
			return newDeclarativeAgent(name, func(a *agentoryv1alpha1.Agent) {
				a.Spec.Runtime = agentoryv1alpha1.RuntimeContainer
				a.Spec.Declarative = nil
				a.Spec.Container = &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/echo:1.0.0"}
			})
		}
		markDeploymentAvailable := func(agent *agentoryv1alpha1.Agent) {
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &deploy)).To(Succeed())
			deploy.Status = appsv1.DeploymentStatus{ObservedGeneration: deploy.Generation, Replicas: 1,
				UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
			Expect(k8sClient.Status().Update(ctx, &deploy)).To(Succeed())
		}
		getRoute := func(agent *agentoryv1alpha1.Agent) (*unstructured.Unstructured, error) {
			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(a2a.HTTPRouteGVK)
			return route, k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), route)
		}

		It("routes the agent through the gateway once the gateway accepts it", func() {
			agent := exposedAgent(uniqueName("echo"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			reconcileAgent(agent)
			markDeploymentAvailable(agent)
			got := reconcileAgent(agent)

			By("waiting on the gateway while the workload is already available")
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionWorkloadReady)).To(BeTrue())
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionGatewayRouteReady)
			Expect(cond.Reason).To(Equal(a2a.ReasonRoutePending))
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse())
			Expect(got.Status.Endpoints.A2A).To(Equal(
				"http://agentory-gateway.agentgateway-system.svc.cluster.local:8080/agents/" + testNamespace + "/" + agent.Name))
			Expect(got.Status.Endpoints.AgentCard).To(Equal(got.Status.Endpoints.A2A + "/.well-known/agent-card.json"))

			By("the card is generated and mounted into the pod")
			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: agent.Name + "-agent-card"}, &cm)).To(Succeed())
			Expect(cm.Data[agentruntime.AgentCardFile]).To(ContainSubstring(got.Status.Endpoints.A2A))
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal(agent.Name + "-agent-card"))
			Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath).To(Equal(agentruntime.AgentCardMountPath))

			By("the gateway accepts the route")
			route, err := getRoute(agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(metav1.IsControlledBy(route, got)).To(BeTrue())
			Expect(unstructured.SetNestedSlice(route.Object, acceptedByGateway(), "status", "parents")).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

			got = reconcileAgent(agent)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionGatewayRouteReady)).To(BeTrue())
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhaseReady))
		})

		It("removes the route when exposure is turned off", func() {
			agent := exposedAgent(uniqueName("private"))
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)
			reconcileAgent(agent)
			_, err := getRoute(agent)
			Expect(err).NotTo(HaveOccurred())

			var current agentoryv1alpha1.Agent
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &current)).To(Succeed())
			current.Spec.A2A = &agentoryv1alpha1.A2ASpec{Expose: new(false)}
			Expect(k8sClient.Update(ctx, &current)).To(Succeed())
			reconcileAgent(agent) // rolls the deployment to the new revision
			markDeploymentAvailable(agent)

			got := reconcileAgent(agent)
			_, err = getRoute(agent)
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred(), "route must be deleted")
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionGatewayRouteReady)).To(BeNil())
			Expect(got.Status.Endpoints.A2A).To(BeEmpty())
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhaseReady), "an unexposed agent is ready without a route")
		})
	})

	Context("with a declarative agent", func() {
		BeforeEach(func() {
			reconciler.Backends = agentruntime.NewRegistry(agentruntime.NewDeploymentBackend(
				k8sClient, k8sClient.Scheme(), agentruntime.DefaultGatewayConfig()).WithDeclarativeImage("runtime:test"))
		})

		It("waits for its skill, then deploys the runtime with it", func() {
			skillName := uniqueName("skill-triage")
			agent := newDeclarativeAgent(uniqueName("oncall"), func(a *agentoryv1alpha1.Agent) {
				a.Spec.Declarative.Skills = []agentoryv1alpha1.SkillRef{{
					Name:   "triage",
					Source: agentoryv1alpha1.ArtifactSource{ConfigMap: &agentoryv1alpha1.ConfigMapArtifact{Name: skillName}},
				}}
			})
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(missingSkillRetry), "poll for the missing skill")
			got := reconcileAgent(agent)
			ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(agentruntime.ReasonSkillNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &appsv1.Deployment{})).NotTo(Succeed())

			skill := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: skillName, Namespace: testNamespace},
				Data:       map[string]string{"SKILL.md": "---\nname: triage\n---\nCheck the pods."},
			}
			Expect(k8sClient.Create(ctx, skill)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, skill)

			got = reconcileAgent(agent)
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("runtime:test"))
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionWorkloadReady).Reason).To(Equal(agentruntime.ReasonProgressing))
			var cfg corev1.ConfigMap
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: agent.Name + "-runtime"}, &cfg)).To(Succeed())
			Expect(cfg.Data[agentruntime.RuntimeConfigFile]).To(ContainSubstring(`"path": "/etc/agentory/skills/triage"`))
		})
	})

	Context("with MCP tools", func() {
		const registry = "agentory-mcp-test"

		BeforeEach(func() {
			cfg := agentruntime.DefaultGatewayConfig()
			reconciler.Backends = agentruntime.NewRegistry(agentruntime.NewDeploymentBackend(k8sClient, k8sClient.Scheme(), cfg))
			reconciler.Tools = mcp.NewManager(k8sClient, k8sClient.Scheme(), cfg, registry)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: registry}}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		})

		toolAgent := func(name, server string) *agentoryv1alpha1.Agent {
			return newDeclarativeAgent(name, func(a *agentoryv1alpha1.Agent) {
				a.Spec.Runtime = agentoryv1alpha1.RuntimeContainer
				a.Spec.Declarative = nil
				a.Spec.Container = &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/agent:1"}
				a.Spec.Tools = &agentoryv1alpha1.ToolsSpec{MCPServers: []agentoryv1alpha1.MCPServerRef{
					{Name: server, Tools: []string{"get_recent_deploys"}}}}
			})
		}
		available := func(agent *agentoryv1alpha1.Agent) {
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &deploy)).To(Succeed())
			deploy.Status = appsv1.DeploymentStatus{ObservedGeneration: deploy.Generation, Replicas: 1,
				UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
			Expect(k8sClient.Status().Update(ctx, &deploy)).To(Succeed())
		}
		toolRoute := func(agent *agentoryv1alpha1.Agent) (*unstructured.Unstructured, error) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(a2a.HTTPRouteGVK)
			return u, k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: agent.Name + "-mcp"}, u)
		}

		It("is not Ready while its MCP server is unknown", func() {
			agent := toolAgent(uniqueName("tools"), "no-such-server")
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)
			reconcileAgent(agent)
			available(agent)
			got := reconcileAgent(agent)

			tools := meta.FindStatusCondition(got.Status.Conditions, ConditionToolsReady)
			Expect(tools.Reason).To(Equal(mcp.ReasonMCPServerNotFound))
			ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(mcp.ReasonMCPServerNotFound))
		})

		It("routes a registry MCP server and becomes Ready once the gateway accepts it", func() {
			server := &agentoryv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: uniqueName("oncall"), Namespace: registry},
				Spec:       agentoryv1alpha1.MCPServerSpec{URL: "http://oncall-mcp.mcp-demo.svc.cluster.local:8080/mcp"},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, server)
			agent := toolAgent(uniqueName("tools"), server.Name)
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)
			reconcileAgent(agent)
			available(agent)

			got := reconcileAgent(agent)
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionToolsReady).Reason).To(Equal(mcp.ReasonToolsPending))
			route, err := toolRoute(agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedSlice(route.Object, acceptedByGateway(), "status", "parents")).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

			got = reconcileAgent(agent)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionToolsReady)).To(BeTrue())
			Expect(got.Status.Phase).To(Equal(agentoryv1alpha1.AgentPhaseReady))

			By("removing the tools removes the route")
			var current agentoryv1alpha1.Agent
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(agent), &current)).To(Succeed())
			current.Spec.Tools = nil
			Expect(k8sClient.Update(ctx, &current)).To(Succeed())
			got = reconcileAgent(agent)
			_, err = toolRoute(agent)
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred())
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionToolsReady)).To(BeNil())
		})
	})
})

// acceptedByGateway is the HTTPRoute status agentgateway writes when it accepts
// a route (envtest runs no gateway controller).
func acceptedByGateway() []any {
	cond := func(t string) map[string]any {
		return map[string]any{"type": t, "status": "True", "reason": t, "message": "",
			"lastTransitionTime": "2026-11-14T00:00:00Z"}
	}
	return []any{map[string]any{
		"parentRef":      map[string]any{"name": "agentory-gateway", "namespace": "agentgateway-system"},
		"controllerName": "agentgateway.dev/agentgateway",
		"conditions":     []any{cond("Accepted"), cond("ResolvedRefs")},
	}}
}
