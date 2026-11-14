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

// Package fake is an in-memory llmgateway.Client for tests.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/ATHUL-B/Agentory/operator/internal/llmgateway"
)

// Gateway is an in-memory llmgateway.Client.
type Gateway struct {
	mu   sync.Mutex
	seq  int
	keys map[string]*entry // by ID

	// Err, if set, is returned by every call.
	Err error
	// Calls counts calls by method name.
	Calls map[string]int
}

type entry struct {
	key  llmgateway.VirtualKey
	spec llmgateway.VirtualKeySpec
}

// New returns an empty gateway.
func New() *Gateway {
	return &Gateway{keys: map[string]*entry{}, Calls: map[string]int{}}
}

func (g *Gateway) record(method string) error {
	g.Calls[method]++
	return g.Err
}

// Get implements llmgateway.Client.
func (g *Gateway) Get(_ context.Context, id string) (*llmgateway.VirtualKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("Get"); err != nil {
		return nil, err
	}
	e, ok := g.keys[id]
	if !ok {
		return nil, llmgateway.ErrNotFound
	}
	k := e.key
	return &k, nil
}

// FindByName implements llmgateway.Client.
func (g *Gateway) FindByName(_ context.Context, name string) (*llmgateway.VirtualKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("FindByName"); err != nil {
		return nil, err
	}
	for _, e := range g.keys {
		if e.key.Name == name {
			k := e.key
			return &k, nil
		}
	}
	return nil, llmgateway.ErrNotFound
}

// Create implements llmgateway.Client.
func (g *Gateway) Create(_ context.Context, spec llmgateway.VirtualKeySpec) (*llmgateway.VirtualKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("Create"); err != nil {
		return nil, err
	}
	for _, e := range g.keys {
		if e.key.Name == spec.Name {
			return nil, llmgateway.ErrConflict
		}
	}
	g.seq++
	k := llmgateway.VirtualKey{ID: fmt.Sprintf("vk-%d", g.seq), Name: spec.Name, Value: fmt.Sprintf("sk-bf-fake-%d", g.seq)}
	g.keys[k.ID] = &entry{key: k, spec: spec}
	out := k
	return &out, nil
}

// Update implements llmgateway.Client.
func (g *Gateway) Update(_ context.Context, id string, spec llmgateway.VirtualKeySpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("Update"); err != nil {
		return err
	}
	e, ok := g.keys[id]
	if !ok {
		return llmgateway.ErrNotFound
	}
	e.spec = spec
	return nil
}

// Delete implements llmgateway.Client.
func (g *Gateway) Delete(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("Delete"); err != nil {
		return err
	}
	delete(g.keys, id)
	return nil
}

// Spec returns the spec stored for a key, for assertions.
func (g *Gateway) Spec(id string) (llmgateway.VirtualKeySpec, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.keys[id]
	if !ok {
		return llmgateway.VirtualKeySpec{}, false
	}
	return e.spec, true
}

// Len returns how many keys exist.
func (g *Gateway) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.keys)
}

// Forget drops a key without the client knowing, as if the gateway lost it.
func (g *Gateway) Forget(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, id)
}
