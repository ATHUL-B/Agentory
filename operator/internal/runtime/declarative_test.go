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

package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const (
	runtimeImage = "agentory-declarative-runtime:dev"
	triageSkill  = "triage"
	skillCM      = "skill-triage-v1"
)

func declarativeAgent(mutate ...func(*agentoryv1alpha1.Agent)) *agentoryv1alpha1.Agent {
	return containerAgent(append([]func(*agentoryv1alpha1.Agent){func(a *agentoryv1alpha1.Agent) {
		a.Spec.Runtime = agentoryv1alpha1.RuntimeDeclarative
		a.Spec.Container = nil
		a.Spec.Declarative = &agentoryv1alpha1.DeclarativeSpec{
			Instructions: "You are the on-call agent.",
			Skills: []agentoryv1alpha1.SkillRef{{
				Name:   triageSkill,
				Source: agentoryv1alpha1.ArtifactSource{ConfigMap: &agentoryv1alpha1.ConfigMapArtifact{Name: skillCM}},
			}},
		}
	}}, mutate...)...)
}

func skillConfigMap(body string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: skillCM, Namespace: testNamespace},
		Data:       map[string]string{"SKILL.md": body},
	}
}

func newDeclarativeBackend(t *testing.T, objs ...client.Object) (*DeploymentBackend, client.Client) {
	t.Helper()
	b, c := newBackend(t, objs...)
	return b.WithDeclarativeImage(runtimeImage), c
}

func TestDeploymentBackendSupportsDeclarativeOnlyWithImage(t *testing.T) {
	if (&DeploymentBackend{}).Supports(declarativeAgent()) {
		t.Error("declarative agents need a runtime image")
	}
	if !(&DeploymentBackend{DeclarativeImage: runtimeImage}).Supports(declarativeAgent()) {
		t.Error("declarative service agents must be supported once an image is set")
	}
}

func TestDeclarativeAgentGetsRuntimeConfigAndSkills(t *testing.T) {
	agent := declarativeAgent()
	b, c := newDeclarativeBackend(t, agent, skillConfigMap("---\nname: triage\n---\nCheck the pods."))
	ctx := context.Background()

	st, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r1"})
	if err != nil || st.Reason != ReasonProgressing {
		t.Fatalf("EnsureAgent = %+v, %v", st, err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testAgent + "-runtime"}, cm); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(cm, agent) {
		t.Error("runtime config must be owned by the agent")
	}
	var cfg runtimeConfig
	if err := json.Unmarshal([]byte(cm.Data[RuntimeConfigFile]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Instructions != "You are the on-call agent." || len(cfg.Skills) != 1 ||
		cfg.Skills[0] != (runtimeConfigSkill{Name: triageSkill, Path: "/etc/agentory/skills/triage"}) {
		t.Errorf("runtime config = %+v", cfg)
	}

	d := get(t, c, &appsv1.Deployment{})
	pod := d.Spec.Template
	if pod.Annotations[AnnotationContentHash] == "" {
		t.Error("pod template needs the content hash")
	}
	ctr := pod.Spec.Containers[0]
	if ctr.Image != runtimeImage || ctr.Ports[0].ContainerPort != defaultServingPort {
		t.Errorf("container = %s:%d", ctr.Image, ctr.Ports[0].ContainerPort)
	}
	if envMap(ctr.Env)[EnvRuntimeConfig].Value != "/etc/agentory/runtime/config.json" {
		t.Errorf("env = %v", envMap(ctr.Env)[EnvRuntimeConfig])
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for _, path := range []string{AgentCardMountPath, RuntimeConfigMountPath, "/etc/agentory/skills/triage"} {
		if m, ok := mounts[path]; !ok || !m.ReadOnly {
			t.Errorf("mount %s missing or writable: %+v", path, m)
		}
	}
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	if vols["skill-0"].ConfigMap.Name != skillCM || vols[runtimeConfigVolume].ConfigMap.Name != testAgent+"-runtime" {
		t.Errorf("volumes = %+v", pod.Spec.Volumes)
	}
}

func TestDeclarativeAgentIsIdempotentAndRollsOnSkillEdits(t *testing.T) {
	agent := declarativeAgent()
	skill := skillConfigMap("v1 instructions")
	b, c := newDeclarativeBackend(t, agent, skill)
	ctx := context.Background()
	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r1"}); err != nil {
		t.Fatal(err)
	}
	first := get(t, c, &appsv1.Deployment{})

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r1"}); err != nil {
		t.Fatal(err)
	}
	if again := get(t, c, &appsv1.Deployment{}); again.ResourceVersion != first.ResourceVersion {
		t.Error("second EnsureAgent rewrote the deployment")
	}

	skill.Data["SKILL.md"] = "v2 instructions"
	if err := c.Update(ctx, skill); err != nil {
		t.Fatal(err)
	}
	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r1"}); err != nil {
		t.Fatal(err)
	}
	after := get(t, c, &appsv1.Deployment{})
	if after.Spec.Template.Annotations[AnnotationContentHash] == first.Spec.Template.Annotations[AnnotationContentHash] {
		t.Error("editing a skill must change the content hash so pods restart")
	}
}

func TestDeclarativeAgentReportsSkillProblems(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		agent  *agentoryv1alpha1.Agent
		objs   []client.Object
		reason string
		text   string
	}{
		"missing ConfigMap": {declarativeAgent(), nil, ReasonSkillNotFound, "not found"},
		"no SKILL.md key": {declarativeAgent(), []client.Object{&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: skillCM, Namespace: testNamespace}, Data: map[string]string{"README": "x"}}},
			ReasonSkillNotFound, "no SKILL.md"},
		"git source": {declarativeAgent(func(a *agentoryv1alpha1.Agent) {
			a.Spec.Declarative.Skills[0].Source = agentoryv1alpha1.ArtifactSource{Git: &agentoryv1alpha1.GitArtifact{
				URL: "https://example.com/s.git", Commit: strings.Repeat("a", 40)}}
		}), nil, ReasonSkillSourceNotSupported, "git sources are not supported yet"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, c := newDeclarativeBackend(t, append(tc.objs, tc.agent)...)
			st, err := b.EnsureAgent(ctx, tc.agent, Deps{Revision: "r"})
			if err != nil || st.WorkloadReady || st.Reason != tc.reason || !strings.Contains(st.Message, tc.text) {
				t.Fatalf("EnsureAgent = %+v, %v; want %s mentioning %q", st, err, tc.reason, tc.text)
			}
			if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testAgent}, &appsv1.Deployment{}); err == nil {
				t.Error("nothing may be deployed while a skill is unavailable")
			}
		})
	}
}

func TestDeclarativeAgentWithoutSkills(t *testing.T) {
	agent := declarativeAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.Declarative.Skills = nil })
	b, c := newDeclarativeBackend(t, agent)
	if _, err := b.EnsureAgent(context.Background(), agent, Deps{Revision: "r"}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testAgent + "-runtime"}, cm)
	if !strings.Contains(cm.Data[RuntimeConfigFile], `"skills": []`) {
		t.Errorf("skills must be an empty list: %s", cm.Data[RuntimeConfigFile])
	}
}
