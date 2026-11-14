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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/ownership"
)

// Declarative runtime layout inside the container.
const (
	RuntimeConfigMountPath = "/etc/agentory/runtime"
	RuntimeConfigFile      = "config.json"
	SkillsMountPath        = "/etc/agentory/skills"
	EnvRuntimeConfig       = "AGENTORY_RUNTIME_CONFIG"

	// AnnotationContentHash on pod templates rolls the workload when mounted
	// content its process reads only at start (runtime config, skills) changes.
	AnnotationContentHash = "agentory.dev/content-hash"
)

// Reasons for declarative agents that cannot run yet.
const (
	ReasonSkillNotFound           = "SkillNotFound"
	ReasonSkillSourceNotSupported = "SkillSourceNotSupported"
)

const runtimeConfigVolume = "runtime-config"

// workload is what differs between the runtimes a DeploymentBackend runs.
type workload struct {
	image       string
	userEnv     []corev1.EnvVar // before the platform environment
	runtimeEnv  []corev1.EnvVar // after it
	volumes     []corev1.Volume
	mounts      []corev1.VolumeMount
	contentHash string
}

// RuntimeConfigName is the ConfigMap holding a declarative agent's runtime config.
func RuntimeConfigName(agent *agentoryv1alpha1.Agent) string {
	return agent.Name + "-runtime"
}

// runtimeConfig is the file the declarative runtime reads at start
// (runtime/declarative/runtime.py, RuntimeConfig.load).
type runtimeConfig struct {
	Instructions string               `json:"instructions"`
	Skills       []runtimeConfigSkill `json:"skills"`
	// MCP turns on tools from AGENTORY_MCP_URL (the Agent lists MCP servers).
	MCP bool `json:"mcp"`
}

type runtimeConfigSkill struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// workload resolves what the Agent's runtime needs. A non-nil status means the
// Agent cannot run yet (for example a missing skill), and nothing is deployed.
func (b *DeploymentBackend) workload(ctx context.Context, agent *agentoryv1alpha1.Agent) (workload, *AgentRuntimeStatus, error) {
	if agent.Spec.Runtime != agentoryv1alpha1.RuntimeDeclarative {
		return workload{image: agent.Spec.Container.Image, userEnv: agent.Spec.Container.Env}, nil, nil
	}
	return b.declarativeWorkload(ctx, agent)
}

func (b *DeploymentBackend) declarativeWorkload(ctx context.Context, agent *agentoryv1alpha1.Agent) (
	workload, *AgentRuntimeStatus, error) {
	wl := workload{
		image:      b.DeclarativeImage,
		runtimeEnv: []corev1.EnvVar{{Name: EnvRuntimeConfig, Value: path.Join(RuntimeConfigMountPath, RuntimeConfigFile)}},
		volumes:    []corev1.Volume{configMapVolume(runtimeConfigVolume, RuntimeConfigName(agent), false)},
		mounts:     []corev1.VolumeMount{{Name: runtimeConfigVolume, MountPath: RuntimeConfigMountPath, ReadOnly: true}},
	}
	cfg := runtimeConfig{Instructions: agent.Spec.Declarative.Instructions, Skills: []runtimeConfigSkill{}, MCP: hasTools(agent)}
	hash := sha256.New()

	for i, skill := range agent.Spec.Declarative.Skills {
		src := skill.Source
		if src.ConfigMap == nil {
			kind := "git"
			if src.OCI != nil {
				kind = "oci"
			}
			return wl, &AgentRuntimeStatus{
				Reason: ReasonSkillSourceNotSupported,
				Message: fmt.Sprintf("skill %q: %s sources are not supported yet; use a configMap source (backlog 8a)",
					skill.Name, kind),
			}, nil
		}
		cm := &corev1.ConfigMap{}
		switch err := b.Client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: src.ConfigMap.Name}, cm); {
		case apierrors.IsNotFound(err):
			return wl, &AgentRuntimeStatus{
				Reason:  ReasonSkillNotFound,
				Message: fmt.Sprintf("skill %q: ConfigMap %s/%s not found", skill.Name, agent.Namespace, src.ConfigMap.Name),
			}, nil
		case err != nil:
			return wl, nil, err
		}
		if _, ok := cm.Data["SKILL.md"]; !ok {
			return wl, &AgentRuntimeStatus{
				Reason:  ReasonSkillNotFound,
				Message: fmt.Sprintf("skill %q: ConfigMap %s has no SKILL.md key", skill.Name, cm.Name),
			}, nil
		}
		hashConfigMap(hash, cm)

		volume := fmt.Sprintf("skill-%d", i)
		mountPath := path.Join(SkillsMountPath, skill.Name)
		wl.volumes = append(wl.volumes, configMapVolume(volume, cm.Name, false))
		wl.mounts = append(wl.mounts, corev1.VolumeMount{Name: volume, MountPath: mountPath, ReadOnly: true})
		cfg.Skills = append(cfg.Skills, runtimeConfigSkill{Name: skill.Name, Path: mountPath})
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return wl, nil, fmt.Errorf("encoding runtime config: %w", err)
	}
	_, _ = hash.Write(data)
	wl.contentHash = hex.EncodeToString(hash.Sum(nil))[:16]

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RuntimeConfigName(agent), Namespace: agent.Namespace}}
	if err := b.ensure(ctx, agent, cm, func() {
		cm.Data = map[string]string{RuntimeConfigFile: string(data)}
	}); err != nil {
		if errors.Is(err, ownership.ErrNotOwned) {
			return wl, &AgentRuntimeStatus{Reason: ReasonResourceConflict, Message: err.Error()}, nil
		}
		return wl, nil, err
	}
	return wl, nil, nil
}

// hashConfigMap adds a ConfigMap's content to h in a stable order.
func hashConfigMap(h interface{ Write([]byte) (int, error) }, cm *corev1.ConfigMap) {
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	_, _ = fmt.Fprintf(h, "configmap:%s\n", cm.Name)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%d:%s\n", k, len(cm.Data[k]), cm.Data[k])
	}
}
