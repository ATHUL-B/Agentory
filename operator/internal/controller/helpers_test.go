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
	"fmt"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const testNamespace = "default"

var nameSeq atomic.Int64

// uniqueName avoids collisions between specs sharing one envtest API server.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nameSeq.Add(1))
}

// newDeclarativeAgent returns a valid declarative service agent; mutate adjusts it per test.
func newDeclarativeAgent(name string, mutate ...func(*agentoryv1alpha1.Agent)) *agentoryv1alpha1.Agent {
	agent := &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: agentoryv1alpha1.AgentSpec{
			Runtime:     agentoryv1alpha1.RuntimeDeclarative,
			Lifecycle:   agentoryv1alpha1.LifecycleService,
			Declarative: &agentoryv1alpha1.DeclarativeSpec{Instructions: "You are a helpful on-call agent."},
			Model:       agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
		},
	}
	for _, m := range mutate {
		m(agent)
	}
	return agent
}

func newTask(name, agentName string) *agentoryv1alpha1.Task {
	return &agentoryv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: agentoryv1alpha1.TaskSpec{
			AgentRef: agentoryv1alpha1.AgentReference{Name: agentName},
			Input:    agentoryv1alpha1.TaskInput{Message: "Investigate alert PD-123"},
		},
	}
}
