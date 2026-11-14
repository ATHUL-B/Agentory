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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

func newScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newBackend(t *testing.T, objs ...client.Object) (*DeploymentBackend, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewDeploymentBackend(c, scheme, DefaultGatewayConfig()), c
}

func get[T client.Object](t *testing.T, c client.Client, obj T) T {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testAgent}, obj); err != nil {
		t.Fatalf("get %T: %v", obj, err)
	}
	return obj
}

func TestEnsureAgentCreatesOwnedResources(t *testing.T) {
	agent := containerAgent(func(a *agentoryv1alpha1.Agent) {
		a.Spec.Container.Env = []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}
	})
	b, c := newBackend(t, agent)

	status, err := b.EnsureAgent(context.Background(), agent, Deps{Revision: testAgent + "-rev1"})
	if err != nil {
		t.Fatal(err)
	}
	if status.WorkloadReady || status.Reason != ReasonProgressing {
		t.Errorf("fresh deployment status = %+v, want Progressing", status)
	}
	if status.InternalEndpoint != "http://crew.team-a.svc.cluster.local:9000" {
		t.Errorf("endpoint = %q", status.InternalEndpoint)
	}

	objs := []client.Object{
		get(t, c, &appsv1.Deployment{}), get(t, c, &corev1.Service{}),
		get(t, c, &corev1.ServiceAccount{}), get(t, c, &networkingv1.NetworkPolicy{}),
	}
	for _, o := range objs {
		if !metav1.IsControlledBy(o, agent) {
			t.Errorf("%T is not controlled by the agent", o)
		}
		if o.GetLabels()[LabelAgent] != testAgent || o.GetLabels()[LabelManagedBy] != ManagedBy {
			t.Errorf("%T labels = %v", o, o.GetLabels())
		}
	}

	d := objs[0].(*appsv1.Deployment)
	pod := d.Spec.Template
	if pod.Annotations[AnnotationRevision] != testAgent+"-rev1" {
		t.Errorf("revision annotation = %q", pod.Annotations[AnnotationRevision])
	}
	if pod.Spec.ServiceAccountName != testAgent || *pod.Spec.AutomountServiceAccountToken || *pod.Spec.EnableServiceLinks {
		t.Errorf("pod identity settings = sa:%q automount:%v links:%v", pod.Spec.ServiceAccountName,
			*pod.Spec.AutomountServiceAccountToken, *pod.Spec.EnableServiceLinks)
	}
	ctr := pod.Spec.Containers[0]
	if ctr.Image != "ghcr.io/example/crew:1.0.0" || ctr.Ports[0].ContainerPort != 9000 {
		t.Errorf("container = %s port %d", ctr.Image, ctr.Ports[0].ContainerPort)
	}
	if ctr.Env[0].Name != "LOG_LEVEL" {
		t.Errorf("user env must come first, got %q", ctr.Env[0].Name)
	}
	if envMap(ctr.Env)[EnvLLMBaseURL].Value == "" {
		t.Error("platform env not injected")
	}
	sc := ctr.SecurityContext
	if *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || len(sc.Capabilities.Add) != 0 {
		t.Errorf("security context = %+v", sc)
	}

	svc := objs[1].(*corev1.Service)
	if svc.Spec.Ports[0].Port != 9000 || svc.Spec.Selector[LabelAgent] != testAgent {
		t.Errorf("service = %+v", svc.Spec)
	}
}

func TestEnsureAgentIsIdempotent(t *testing.T) {
	agent := containerAgent()
	b, c := newBackend(t, agent)
	ctx := context.Background()
	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: testAgent + "-rev1"}); err != nil {
		t.Fatal(err)
	}
	before := get(t, c, &appsv1.Deployment{}).ResourceVersion

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: testAgent + "-rev1"}); err != nil {
		t.Fatal(err)
	}
	if after := get(t, c, &appsv1.Deployment{}).ResourceVersion; after != before {
		t.Errorf("second EnsureAgent rewrote the deployment (%s -> %s)", before, after)
	}

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: testAgent + "-rev2"}); err != nil {
		t.Fatal(err)
	}
	d := get(t, c, &appsv1.Deployment{})
	if d.Spec.Template.Annotations[AnnotationRevision] != testAgent+"-rev2" {
		t.Errorf("new revision not rolled out: %v", d.Spec.Template.Annotations)
	}
}

func TestEnsureAgentReportsReadyWhenAvailable(t *testing.T) {
	agent := containerAgent()
	b, c := newBackend(t, agent)
	ctx := context.Background()
	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r"}); err != nil {
		t.Fatal(err)
	}
	d := get(t, c, &appsv1.Deployment{})
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, UpdatedReplicas: 1, AvailableReplicas: 1}
	if d.Generation == 0 {
		// The fake client does not bump generation; mirror the API server.
		d.Generation = 1
		d.Status.ObservedGeneration = 1
		if err := c.Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		d = get(t, c, &appsv1.Deployment{})
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, UpdatedReplicas: 1, AvailableReplicas: 1}
	}
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}

	status, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.WorkloadReady || status.Reason != ReasonAvailable {
		t.Errorf("status = %+v, want ready", status)
	}
}

func TestEnsureAgentRefusesToAdoptForeignObjects(t *testing.T) {
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testAgent, Namespace: testNamespace}}
	agent := containerAgent()
	b, c := newBackend(t, agent, foreign)

	status, err := b.EnsureAgent(context.Background(), agent, Deps{Revision: "r"})
	if err != nil {
		t.Fatalf("conflict must be reported in status, not as an error: %v", err)
	}
	if status.Reason != ReasonResourceConflict || status.WorkloadReady {
		t.Fatalf("status = %+v, want ResourceConflict", status)
	}
	svc := get(t, c, &corev1.Service{})
	if len(svc.OwnerReferences) != 0 || svc.Labels[LabelManagedBy] != "" {
		t.Errorf("foreign service was modified: %+v", svc.ObjectMeta)
	}
}

func TestEnsureAgentPrivilegedPortAddsBindCapability(t *testing.T) {
	agent := containerAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.Container.Port = 80 })
	b, c := newBackend(t, agent)
	if _, err := b.EnsureAgent(context.Background(), agent, Deps{Revision: "r"}); err != nil {
		t.Fatal(err)
	}
	caps := get(t, c, &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].SecurityContext.Capabilities
	if len(caps.Add) != 1 || caps.Add[0] != "NET_BIND_SERVICE" {
		t.Errorf("capabilities = %+v, want NET_BIND_SERVICE added for port 80", caps)
	}
}

func TestEnsureAgentRollsPodsWhenVirtualKeyChanges(t *testing.T) {
	agent := containerAgent()
	b, c := newBackend(t, agent)
	ctx := context.Background()

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r", VirtualKeyID: "vk-1"}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, c, &appsv1.Deployment{}).Spec.Template.Annotations[AnnotationVirtualKeyID]; got != "vk-1" {
		t.Fatalf("virtual key annotation = %q, want vk-1", got)
	}

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r", VirtualKeyID: "vk-2"}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, c, &appsv1.Deployment{}).Spec.Template.Annotations[AnnotationVirtualKeyID]; got != "vk-2" {
		t.Errorf("rotated key not rolled out: annotation = %q", got)
	}

	if _, err := b.EnsureAgent(ctx, agent, Deps{Revision: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, c, &appsv1.Deployment{}).Spec.Template.Annotations[AnnotationVirtualKeyID]; ok {
		t.Error("annotation must be removed when keys are not managed")
	}
}
