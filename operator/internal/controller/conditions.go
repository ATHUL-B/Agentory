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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

// Condition types.
const (
	ConditionAccepted      = "Accepted"
	ConditionReady         = "Ready"
	ConditionAgentResolved = "AgentResolved"
	ConditionStarted       = "Started"
)

// Condition reasons.
const (
	ReasonSpecValid             = "SpecValid"
	ReasonBackendNotImplemented = "BackendNotImplemented"
	ReasonAgentFound            = "AgentFound"
	ReasonAgentNotFound         = "AgentNotFound"
)

// revisionHashLength keeps revisions short enough to read in kubectl output.
const revisionHashLength = 10

// SpecRevision returns "<name>-<hash>", where hash is derived from the spec.
// Equal specs always give the same revision; any spec change gives a new one.
// Tasks record the revision they ran against (HLD-0001 §3.2).
func SpecRevision(name string, spec *agentoryv1alpha1.AgentSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encoding agent spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%s", name, hex.EncodeToString(sum[:])[:revisionHashLength]), nil
}
