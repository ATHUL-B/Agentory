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

// Package virtualkey gives every Agent its own LLM gateway virtual key
// (backlog item 5). The key allows only the Agent's model, is stored in the
// Agent-owned Secret its workload already reads (runtime.VirtualKeySecretName),
// and is deleted from the gateway when the Agent is deleted.
package virtualkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/llmgateway"
	agentruntime "github.com/ATHUL-B/Agentory/operator/internal/runtime"
)

// Annotations on the key Secret.
const (
	AnnotationKeyID   = "agentory.dev/virtual-key-id"
	AnnotationKeySpec = "agentory.dev/virtual-key-spec"
)

// Condition reasons for VirtualKeyReady.
const (
	ReasonKeyReady         = "KeyReady"
	ReasonKeyRotated       = "KeyRotated"
	ReasonGatewayError     = "GatewayError"
	ReasonResourceConflict = "ResourceConflict"
)

// Status reports the Agent's key.
type Status struct {
	Ready   bool
	Reason  string
	Message string
	// KeyID is the gateway's ID for the key. It changes only when the key is re-created.
	KeyID string
}

// Manager reconciles one virtual key per Agent.
type Manager struct {
	// Client must read Secrets from the API server, not a cache.
	Client  client.Client
	Scheme  *k8sruntime.Scheme
	Gateway llmgateway.Client
}

// KeyName is the Agent's key name in the gateway; unique per namespace/agent.
func KeyName(agent *agentoryv1alpha1.Agent) string {
	return fmt.Sprintf("agentory:%s/%s", agent.Namespace, agent.Name)
}

// SpecFor is what the Agent's key allows: only the Agent's model.
func SpecFor(agent *agentoryv1alpha1.Agent) llmgateway.VirtualKeySpec {
	return llmgateway.VirtualKeySpec{
		Name:        KeyName(agent),
		Description: fmt.Sprintf("Agentory agent %s/%s (managed by agentory-operator)", agent.Namespace, agent.Name),
		Provider:    agent.Spec.Model.Provider,
		Model:       agent.Spec.Model.Name,
		Metadata: map[string]string{
			"agentory.agent":     agent.Name,
			"agentory.namespace": agent.Namespace,
			"agentory.uid":       string(agent.UID),
		},
	}
}

func specHash(spec llmgateway.VirtualKeySpec) string {
	// Metadata includes the UID, so a re-created Agent gets its key updated too.
	data, _ := json.Marshal(spec)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// Ensure makes the Agent's key exist in the gateway with the current spec and
// stores it in the Agent's Secret. Gateway failures are returned as errors so
// the reconcile retries; a name clash with a foreign Secret is reported in Status.
func (m *Manager) Ensure(ctx context.Context, agent *agentoryv1alpha1.Agent) (Status, error) {
	spec := SpecFor(agent)
	hash := specHash(spec)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: agentruntime.VirtualKeySecretName(agent), Namespace: agent.Namespace,
	}}
	existing := &corev1.Secret{}
	switch err := m.Client.Get(ctx, client.ObjectKeyFromObject(secret), existing); {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return Status{}, err
	case !metav1.IsControlledBy(existing, agent):
		return Status{
			Reason:  ReasonResourceConflict,
			Message: fmt.Sprintf("Secret %s/%s exists and is not managed by this Agent", secret.Namespace, secret.Name),
		}, nil
	}

	id, value, storedHash := "", "", ""
	if existing != nil {
		id = existing.Annotations[AnnotationKeyID]
		value = string(existing.Data[agentruntime.VirtualKeySecretKey])
		storedHash = existing.Annotations[AnnotationKeySpec]
	}

	reason := ReasonKeyReady
	if id != "" {
		switch _, err := m.Gateway.Get(ctx, id); {
		case errors.Is(err, llmgateway.ErrNotFound):
			// The gateway lost the key (e.g. its database was reset): re-create it.
			id, value, reason = "", "", ReasonKeyRotated
		case err != nil:
			return gatewayError(err)
		}
	}

	if id == "" || value == "" {
		key, err := m.create(ctx, spec)
		if err != nil {
			return gatewayError(err)
		}
		id, value, storedHash = key.ID, key.Value, hash
	} else if storedHash != hash {
		if err := m.Gateway.Update(ctx, id, spec); err != nil {
			return gatewayError(err)
		}
		storedHash = hash
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, m.Client, secret, func() error {
		secret.Labels = agentruntime.ObjectLabels(agent)
		secret.Annotations = map[string]string{AnnotationKeyID: id, AnnotationKeySpec: storedHash}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{agentruntime.VirtualKeySecretKey: []byte(value)}
		return controllerutil.SetControllerReference(agent, secret, m.Scheme)
	}); err != nil {
		return Status{}, fmt.Errorf("writing secret %s: %w", secret.Name, err)
	}

	msg := fmt.Sprintf("virtual key %s allows %s/%s; stored in Secret %s", id, spec.Provider, spec.Model, secret.Name)
	if reason == ReasonKeyRotated {
		msg = "gateway had lost the key; re-created it: " + msg
	}
	return Status{Ready: true, Reason: reason, Message: msg, KeyID: id}, nil
}

// create makes the key, replacing an orphan with the same name (left behind
// when the Secret was lost or a previous write failed after creation).
func (m *Manager) create(ctx context.Context, spec llmgateway.VirtualKeySpec) (*llmgateway.VirtualKey, error) {
	key, err := m.Gateway.Create(ctx, spec)
	if !errors.Is(err, llmgateway.ErrConflict) {
		return key, err
	}
	orphan, err := m.Gateway.FindByName(ctx, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("finding orphaned key %q: %w", spec.Name, err)
	}
	if err := m.Gateway.Delete(ctx, orphan.ID); err != nil {
		return nil, fmt.Errorf("deleting orphaned key %q: %w", spec.Name, err)
	}
	return m.Gateway.Create(ctx, spec)
}

// Cleanup deletes the Agent's key from the gateway. The Secret goes with the
// Agent through garbage collection.
func (m *Manager) Cleanup(ctx context.Context, agent *agentoryv1alpha1.Agent) error {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: agent.Namespace, Name: agentruntime.VirtualKeySecretName(agent)}
	switch err := m.Client.Get(ctx, key, secret); {
	case err == nil && metav1.IsControlledBy(secret, agent):
		if id := secret.Annotations[AnnotationKeyID]; id != "" {
			if err := m.Gateway.Delete(ctx, id); err != nil {
				return fmt.Errorf("deleting virtual key %s: %w", id, err)
			}
		}
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	// Also catch a key whose Secret was never written or was lost.
	switch orphan, err := m.Gateway.FindByName(ctx, KeyName(agent)); {
	case errors.Is(err, llmgateway.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("finding virtual key %q: %w", KeyName(agent), err)
	default:
		return m.Gateway.Delete(ctx, orphan.ID)
	}
}

func gatewayError(err error) (Status, error) {
	return Status{Reason: ReasonGatewayError, Message: err.Error()}, err
}
