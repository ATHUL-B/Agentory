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
	"errors"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// Condition reasons reported by DeploymentBackend.
const (
	ReasonAvailable        = "Available"
	ReasonProgressing      = "Progressing"
	ReasonResourceConflict = "ResourceConflict"
)

const (
	containerName      = "agent"
	portName           = "a2a"
	defaultServingPort = int32(8080)
	privilegedPortMax  = 1024
)

// ErrNotOwned means an object with the name the backend needs already exists
// and is not controlled by the Agent. The backend never adopts such objects.
var ErrNotOwned = errors.New("exists and is not managed by this Agent")

// DeploymentBackend runs long-lived container agents (runtime "container",
// lifecycle "service") as a Deployment, Service, ServiceAccount and NetworkPolicy,
// all named after the Agent and owned by it.
type DeploymentBackend struct {
	Client client.Client
	Scheme *k8sruntime.Scheme
	Env    EnvBuilder
	Policy PolicyBuilder
}

// NewDeploymentBackend wires a DeploymentBackend to the given gateways.
func NewDeploymentBackend(c client.Client, scheme *k8sruntime.Scheme, cfg GatewayConfig) *DeploymentBackend {
	return &DeploymentBackend{Client: c, Scheme: scheme, Env: EnvBuilder{Config: cfg}, Policy: PolicyBuilder{Config: cfg}}
}

// Name implements Backend.
func (b *DeploymentBackend) Name() string { return "deployment" }

// Supports implements Backend. Declarative service agents join with backlog item 8.
func (b *DeploymentBackend) Supports(agent *agentoryv1alpha1.Agent) bool {
	return agent.Spec.Runtime == agentoryv1alpha1.RuntimeContainer &&
		agent.Spec.Lifecycle == agentoryv1alpha1.LifecycleService &&
		agent.Spec.Container != nil
}

// EnsureAgent implements Backend.
func (b *DeploymentBackend) EnsureAgent(ctx context.Context, agent *agentoryv1alpha1.Agent, revision string) (AgentRuntimeStatus, error) {
	port := servingPort(agent)
	meta := func() metav1.ObjectMeta { return metav1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace} }

	sa := &corev1.ServiceAccount{ObjectMeta: meta()}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: meta()}
	svc := &corev1.Service{ObjectMeta: meta()}
	deploy := &appsv1.Deployment{ObjectMeta: meta()}

	steps := []struct {
		obj    client.Object
		mutate func()
	}{
		{sa, func() { sa.AutomountServiceAccountToken = new(false) }},
		{policy, func() { policy.Spec = b.Policy.Build(agent, port) }},
		{svc, func() { b.mutateService(svc, agent, port) }},
		{deploy, func() { b.mutateDeployment(deploy, agent, revision, port) }},
	}
	for _, step := range steps {
		if err := b.ensure(ctx, agent, step.obj, step.mutate); err != nil {
			if errors.Is(err, ErrNotOwned) {
				return AgentRuntimeStatus{Reason: ReasonResourceConflict, Message: err.Error()}, nil
			}
			return AgentRuntimeStatus{}, err
		}
	}

	status := deploymentStatus(deploy)
	status.InternalEndpoint = fmt.Sprintf("http://%s.%s.svc.%s:%d", agent.Name, agent.Namespace, b.Env.Config.ClusterDomain, port)
	return status, nil
}

// ensure creates or updates obj, owned by the Agent, refusing to take over an
// object someone else created.
func (b *DeploymentBackend) ensure(ctx context.Context, owner *agentoryv1alpha1.Agent, obj client.Object, mutate func()) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("unexpected object type %T", obj)
	}
	switch err := b.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !metav1.IsControlledBy(existing, owner):
		kind := fmt.Sprintf("%T", obj)
		return fmt.Errorf("%s %s/%s %w", kind, obj.GetNamespace(), obj.GetName(), ErrNotOwned)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, b.Client, obj, func() error {
		obj.SetLabels(mergeLabels(obj.GetLabels(), ObjectLabels(owner)))
		mutate()
		return controllerutil.SetControllerReference(owner, obj, b.Scheme)
	})
	return err
}

func (b *DeploymentBackend) mutateService(svc *corev1.Service, agent *agentoryv1alpha1.Agent, port int32) {
	// Only set the fields we own; the API server fills clusterIP and friends.
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Selector = SelectorLabels(agent)
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       portName,
		Protocol:   corev1.ProtocolTCP,
		Port:       port,
		TargetPort: intstr.FromInt32(port),
	}}
}

func (b *DeploymentBackend) mutateDeployment(d *appsv1.Deployment, agent *agentoryv1alpha1.Agent, revision string, port int32) {
	if d.Spec.Replicas == nil {
		d.Spec.Replicas = new(int32(1))
	}
	d.Spec.Selector = &metav1.LabelSelector{MatchLabels: SelectorLabels(agent)}

	tpl := &d.Spec.Template
	tpl.Labels = mergeLabels(tpl.Labels, ObjectLabels(agent))
	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[AnnotationRevision] = revision

	spec := &tpl.Spec
	spec.ServiceAccountName = agent.Name
	spec.AutomountServiceAccountToken = new(false)
	// Service links would leak every Service in the namespace into the environment.
	spec.EnableServiceLinks = new(false)
	spec.SecurityContext = &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	container := b.container(agent, revision, port)
	if len(spec.Containers) == 1 && spec.Containers[0].Name == containerName {
		// Mutate in place so fields the API server defaulted are kept and
		// reconciles converge without a write.
		existing := &spec.Containers[0]
		existing.Image = container.Image
		existing.Ports = container.Ports
		existing.Env = container.Env
		existing.Resources = container.Resources
		existing.ReadinessProbe = mergeProbe(existing.ReadinessProbe, container.ReadinessProbe)
		existing.SecurityContext = container.SecurityContext
	} else {
		spec.Containers = []corev1.Container{container}
	}
}

func (b *DeploymentBackend) container(agent *agentoryv1alpha1.Agent, revision string, port int32) corev1.Container {
	c := agent.Spec.Container
	env := append([]corev1.EnvVar{}, c.Env...)
	env = append(env, b.Env.Build(agent, revision)...)

	caps := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	if port < privilegedPortMax {
		caps.Add = []corev1.Capability{"NET_BIND_SERVICE"}
	}

	var resources corev1.ResourceRequirements
	if agent.Spec.Resources != nil {
		resources = *agent.Spec.Resources.DeepCopy()
	}

	return corev1.Container{
		Name:      containerName,
		Image:     c.Image,
		Ports:     []corev1.ContainerPort{{Name: portName, ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		Env:       env,
		Resources: resources,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
			PeriodSeconds: 5,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			Capabilities:             caps,
		},
	}
}

// mergeProbe keeps API-server defaults (thresholds, timeouts) from the existing
// probe while applying the handler and period we manage.
func mergeProbe(existing, desired *corev1.Probe) *corev1.Probe {
	if existing == nil {
		return desired
	}
	merged := existing.DeepCopy()
	merged.ProbeHandler = desired.ProbeHandler
	merged.PeriodSeconds = desired.PeriodSeconds
	return merged
}

func deploymentStatus(d *appsv1.Deployment) AgentRuntimeStatus {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	current := s.ObservedGeneration >= d.Generation && d.Generation > 0
	ready := current && s.UpdatedReplicas >= want && s.AvailableReplicas >= want
	msg := fmt.Sprintf("deployment %s: %d/%d available, %d updated", d.Name, s.AvailableReplicas, want, s.UpdatedReplicas)
	if ready {
		return AgentRuntimeStatus{WorkloadReady: true, Reason: ReasonAvailable, Message: msg}
	}
	return AgentRuntimeStatus{Reason: ReasonProgressing, Message: msg}
}

func servingPort(agent *agentoryv1alpha1.Agent) int32 {
	if agent.Spec.Container != nil && agent.Spec.Container.Port > 0 {
		return agent.Spec.Container.Port
	}
	return defaultServingPort
}

func mergeLabels(existing, add map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(add))
	maps.Copy(out, existing)
	maps.Copy(out, add)
	return out
}
