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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const labelNamespaceName = "kubernetes.io/metadata.name"

// PolicyBuilder produces the network lockdown for an agent workload (ADR-0002 §3):
//   - egress only to cluster DNS, the LLM gateway, the agent gateway and the collector;
//   - ingress only from the agent gateway, on the agent's port (when it serves one).
type PolicyBuilder struct {
	Config GatewayConfig
}

// Build returns the NetworkPolicy spec for the Agent. servingPort is the port the
// workload listens on, or 0 if it serves nothing (per-task agents).
func (b PolicyBuilder) Build(agent *agentoryv1alpha1.Agent, servingPort int32) networkingv1.NetworkPolicySpec {
	cfg := b.Config
	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: SelectorLabels(agent)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			b.rule(cfg.DNSNamespace, cfg.DNSPodLabels, tcpUDP(53)...),
			b.rule(cfg.LLMGateway.Namespace, nil, tcp(cfg.LLMGateway.Port)),
			b.rule(cfg.AgentGateway.Namespace, nil, tcp(cfg.AgentGateway.Port)),
			b.rule(cfg.Collector.Namespace, cfg.CollectorPodLabels, tcp(cfg.Collector.Port), tcp(cfg.CollectorGRPCPort)),
		},
		// No ingress rules means no ingress at all.
		Ingress: []networkingv1.NetworkPolicyIngressRule{},
	}
	if servingPort > 0 {
		spec.Ingress = append(spec.Ingress, networkingv1.NetworkPolicyIngressRule{
			From:  []networkingv1.NetworkPolicyPeer{namespacePeer(cfg.AgentGateway.Namespace, nil)},
			Ports: []networkingv1.NetworkPolicyPort{tcp(servingPort)},
		})
	}
	return spec
}

func (b PolicyBuilder) rule(namespace string, podLabels map[string]string, ports ...networkingv1.NetworkPolicyPort) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{namespacePeer(namespace, podLabels)},
		Ports: ports,
	}
}

func namespacePeer(namespace string, podLabels map[string]string) networkingv1.NetworkPolicyPeer {
	peer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespaceName: namespace}},
	}
	if len(podLabels) > 0 {
		peer.PodSelector = &metav1.LabelSelector{MatchLabels: podLabels}
	}
	return peer
}

func tcp(port int32) networkingv1.NetworkPolicyPort {
	return port32(corev1.ProtocolTCP, port)
}

func tcpUDP(port int32) []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{port32(corev1.ProtocolUDP, port), port32(corev1.ProtocolTCP, port)}
}

func port32(protocol corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &p}
}
