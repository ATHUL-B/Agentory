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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const testSkillName = "triage"

// These specs exercise the CRD schemas and CEL rules against a real API server
// (envtest), so they test exactly what the cluster enforces.
var _ = Describe("CRD validation", func() {
	harness := func(a *agentoryv1alpha1.Agent) {
		a.Spec.Runtime = agentoryv1alpha1.RuntimeHarness
		a.Spec.Lifecycle = agentoryv1alpha1.LifecyclePerTask
		a.Spec.Declarative = nil
		a.Spec.Harness = &agentoryv1alpha1.HarnessSpec{Kind: agentoryv1alpha1.HarnessKindClaudeCode}
	}
	container := func(a *agentoryv1alpha1.Agent) {
		a.Spec.Runtime = agentoryv1alpha1.RuntimeContainer
		a.Spec.Declarative = nil
		a.Spec.Container = &agentoryv1alpha1.ContainerSpec{Image: "ghcr.io/example/agent:1.0.0"}
	}
	gitSkill := func(commit string) func(*agentoryv1alpha1.Agent) {
		return func(a *agentoryv1alpha1.Agent) {
			a.Spec.Declarative.Skills = []agentoryv1alpha1.SkillRef{{
				Name: testSkillName,
				Source: agentoryv1alpha1.ArtifactSource{Git: &agentoryv1alpha1.GitArtifact{
					URL: "https://github.com/example/skills.git", Commit: commit,
				}},
			}}
		}
	}
	const validCommit = "0123456789abcdef0123456789abcdef01234567"

	Context("Agent: accepted", func() {
		DescribeTable("valid agents are created",
			func(mutate func(*agentoryv1alpha1.Agent)) {
				agent := newDeclarativeAgent(uniqueName("valid"), mutate)
				Expect(k8sClient.Create(ctx, agent)).To(Succeed())
				DeferCleanup(k8sClient.Delete, ctx, agent)
			},
			Entry("declarative service", func(*agentoryv1alpha1.Agent) {}),
			Entry("declarative perTask", func(a *agentoryv1alpha1.Agent) { a.Spec.Lifecycle = agentoryv1alpha1.LifecyclePerTask }),
			Entry("harness perTask", harness),
			Entry("container service", container),
			Entry("container with its own env", func(a *agentoryv1alpha1.Agent) {
				container(a)
				a.Spec.Container.Env = []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}
			}),
			Entry("skill pinned to a git commit", gitSkill(validCommit)),
			Entry("skill pinned to an OCI digest", func(a *agentoryv1alpha1.Agent) {
				a.Spec.Declarative.Skills = []agentoryv1alpha1.SkillRef{{
					Name: testSkillName,
					Source: agentoryv1alpha1.ArtifactSource{OCI: &agentoryv1alpha1.OCIArtifact{
						Ref: "ghcr.io/example/skills/triage@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
					}},
				}}
			}),
			Entry("user-supplied MCP endpoint", func(a *agentoryv1alpha1.Agent) {
				a.Spec.Tools = &agentoryv1alpha1.ToolsSpec{MCPServers: []agentoryv1alpha1.MCPServerRef{
					{Name: "datadog", Tools: []string{"query_logs"}},
					{Name: "team-runbooks", URL: "https://runbooks.payments.internal/mcp"},
				}}
			}),
		)

		It("applies defaults", func() {
			agent := newDeclarativeAgent(uniqueName("defaults"), container, func(a *agentoryv1alpha1.Agent) {
				a.Spec.A2A = &agentoryv1alpha1.A2ASpec{}
				a.Spec.Observability = &agentoryv1alpha1.ObservabilitySpec{}
			})
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, agent)

			Expect(agent.Spec.Container.Port).To(Equal(int32(8080)))
			Expect(agent.Spec.Container.Protocol).To(Equal(agentoryv1alpha1.ContainerProtocol("a2a")))
			Expect(agent.Spec.A2A.Expose).To(Equal(new(true)))
			Expect(agent.Spec.Observability.Sink).To(Equal(agentoryv1alpha1.TraceSink("default")))
		})
	})

	Context("Agent: rejected", func() {
		DescribeTable("invalid agents are rejected with a clear message",
			func(mutate func(*agentoryv1alpha1.Agent), message string) {
				err := k8sClient.Create(ctx, newDeclarativeAgent(uniqueName("invalid"), mutate))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(message))
			},
			Entry("declarative without spec.declarative",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Declarative = nil },
				"runtime 'declarative' requires spec.declarative"),
			Entry("declarative with a harness block too",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Harness = &agentoryv1alpha1.HarnessSpec{Kind: "codex"} },
				"runtime 'declarative' requires spec.declarative and no spec.harness"),
			Entry("harness without spec.harness",
				func(a *agentoryv1alpha1.Agent) {
					harness(a)
					a.Spec.Harness = nil
				},
				"runtime 'harness' requires spec.harness"),
			Entry("harness as a long-lived service",
				func(a *agentoryv1alpha1.Agent) {
					harness(a)
					a.Spec.Lifecycle = agentoryv1alpha1.LifecycleService
				},
				"runtime 'harness' requires lifecycle 'perTask'"),
			Entry("container without spec.container",
				func(a *agentoryv1alpha1.Agent) {
					container(a)
					a.Spec.Container = nil
				},
				"runtime 'container' requires spec.container"),
			Entry("suspendable lifecycle (Substrate deferred)",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Lifecycle = agentoryv1alpha1.LifecycleSuspendable },
				"lifecycle 'suspendable' is not supported yet"),
			Entry("unknown runtime",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Runtime = "lambda" },
				"Unsupported value: \"lambda\""),
			Entry("unknown harness kind",
				func(a *agentoryv1alpha1.Agent) {
					harness(a)
					a.Spec.Harness.Kind = "copilot"
				},
				"Unsupported value: \"copilot\""),
			Entry("empty instructions",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Declarative.Instructions = "" },
				"spec.declarative.instructions"),
			Entry("skill pinned to a branch instead of a commit",
				gitSkill("main"),
				"spec.declarative.skills[0].source.git.commit"),
			Entry("skill with both git and oci",
				func(a *agentoryv1alpha1.Agent) {
					gitSkill(validCommit)(a)
					a.Spec.Declarative.Skills[0].Source.OCI = &agentoryv1alpha1.OCIArtifact{
						Ref: "ghcr.io/x@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
					}
				},
				"exactly one of git or oci must be set"),
			Entry("OCI skill without a digest",
				func(a *agentoryv1alpha1.Agent) {
					a.Spec.Declarative.Skills = []agentoryv1alpha1.SkillRef{{
						Name:   testSkillName,
						Source: agentoryv1alpha1.ArtifactSource{OCI: &agentoryv1alpha1.OCIArtifact{Ref: "ghcr.io/x:latest"}},
					}}
				},
				"spec.declarative.skills[0].source.oci.ref"),
			Entry("container port out of range",
				func(a *agentoryv1alpha1.Agent) {
					container(a)
					a.Spec.Container.Port = 70000
				},
				"spec.container.port"),
			Entry("MCP endpoint that is not http(s)",
				func(a *agentoryv1alpha1.Agent) {
					a.Spec.Tools = &agentoryv1alpha1.ToolsSpec{MCPServers: []agentoryv1alpha1.MCPServerRef{
						{Name: "bad", URL: "ftp://example.com/mcp"},
					}}
				},
				"spec.tools.mcpServers[0].url"),
			Entry("container env overriding a platform variable",
				func(a *agentoryv1alpha1.Agent) {
					container(a)
					a.Spec.Container.Env = []corev1.EnvVar{{Name: "OPENAI_BASE_URL", Value: "https://api.openai.com"}}
				},
				"reserved for the platform"),
			Entry("container env using the AGENTORY_ prefix",
				func(a *agentoryv1alpha1.Agent) {
					container(a)
					a.Spec.Container.Env = []corev1.EnvVar{{Name: "AGENTORY_MODEL", Value: "x"}}
				},
				"reserved for the platform"),
			Entry("missing model",
				func(a *agentoryv1alpha1.Agent) { a.Spec.Model = agentoryv1alpha1.ModelSpec{} },
				"spec.model.provider"),
		)
	})

	Context("Task", func() {
		It("defaults the timeout to 30 minutes", func() {
			task := newTask(uniqueName("task"), "any-agent")
			Expect(k8sClient.Create(ctx, task)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, task)
			Expect(task.Spec.Timeout).To(Equal(&metav1.Duration{Duration: 30 * time.Minute}))
		})

		It("rejects an empty message", func() {
			task := newTask(uniqueName("task"), "any-agent")
			task.Spec.Input.Message = ""
			err := k8sClient.Create(ctx, task)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.input.message"))
		})

		It("keeps the spec immutable after creation", func() {
			task := newTask(uniqueName("task"), "any-agent")
			Expect(k8sClient.Create(ctx, task)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, task)

			task.Spec.Input.Message = "Do something else"
			err := k8sClient.Update(ctx, task)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Task spec is immutable"))
		})
	})
})
