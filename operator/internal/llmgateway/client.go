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

// Package llmgateway manages per-agent virtual keys in the LLM gateway
// (ADR-0002, HLD-0001 §4 LLMGatewayClient). Client is the seam; Bifrost is
// the implementation in use.
package llmgateway

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the virtual key does not exist in the gateway.
	ErrNotFound = errors.New("virtual key not found")
	// ErrConflict means a virtual key with the same name already exists.
	ErrConflict = errors.New("virtual key name already exists")
)

// VirtualKeySpec is what a virtual key allows.
type VirtualKeySpec struct {
	// Name is unique in the gateway.
	Name        string
	Description string
	// Provider and Model are the only model the key may call.
	Provider string
	Model    string
	// Metadata is attached to the key and to every log entry it produces.
	Metadata map[string]string
}

// VirtualKey is a key as stored in the gateway.
type VirtualKey struct {
	ID    string
	Name  string
	Value string
}

// Client manages virtual keys. Implementations must be safe for concurrent use.
type Client interface {
	// Get returns the key, or ErrNotFound.
	Get(ctx context.Context, id string) (*VirtualKey, error)
	// FindByName returns the key with that name, or ErrNotFound.
	FindByName(ctx context.Context, name string) (*VirtualKey, error)
	// Create makes a new key and returns it with its generated ID and value.
	// It returns ErrConflict if the name is taken.
	Create(ctx context.Context, spec VirtualKeySpec) (*VirtualKey, error)
	// Update replaces what the key allows; its ID and value are unchanged.
	Update(ctx context.Context, id string, spec VirtualKeySpec) error
	// Delete removes the key. Deleting a missing key is not an error.
	Delete(ctx context.Context, id string) error
}
