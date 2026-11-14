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

package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	virtualKeysPath = "/api/governance/virtual-keys"
	defaultTimeout  = 10 * time.Second
	maxErrorBody    = 512
)

// BifrostClient talks to Bifrost's governance API with HTTP Basic auth
// (Bifrost v2.3: POST/GET/PUT/DELETE /api/governance/virtual-keys[/{id}]).
type BifrostClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// NewBifrostClient returns a client for the Bifrost at baseURL (e.g.
// http://bifrost.bifrost.svc.cluster.local:8080) using admin credentials.
func NewBifrostClient(baseURL, username, password string, httpClient *http.Client) *BifrostClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &BifrostClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     httpClient,
	}
}

// bifrostKey is the subset of Bifrost's virtual key we read.
type bifrostKey struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Value secretValue `json:"value"`
}

// secretValue accepts Bifrost's value either as a plain string or as its
// SecretVar object form {"value": "..."}.
type secretValue string

func (s *secretValue) UnmarshalJSON(data []byte) error {
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		*s = secretValue(plain)
		return nil
	}
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("virtual key value: %w", err)
	}
	*s = secretValue(obj.Value)
	return nil
}

func (k bifrostKey) toVirtualKey() *VirtualKey {
	return &VirtualKey{ID: k.ID, Name: k.Name, Value: string(k.Value)}
}

type providerConfig struct {
	Provider      string   `json:"provider"`
	Weight        float64  `json:"weight"`
	AllowedModels []string `json:"allowed_models"`
	KeyIDs        []string `json:"key_ids"`
}

type writeRequest struct {
	Name            string            `json:"name,omitempty"`
	Description     string            `json:"description"`
	ProviderConfigs []providerConfig  `json:"provider_configs"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	IsActive        bool              `json:"is_active"`
}

func toWriteRequest(spec VirtualKeySpec, includeName bool) writeRequest {
	req := writeRequest{
		Description: spec.Description,
		// Least privilege: exactly the agent's model, through any of the
		// gateway's keys for that provider.
		ProviderConfigs: []providerConfig{{
			Provider: spec.Provider, Weight: 1, AllowedModels: []string{spec.Model}, KeyIDs: []string{"*"},
		}},
		Metadata: spec.Metadata,
		IsActive: true,
	}
	if includeName {
		req.Name = spec.Name
	}
	return req
}

// Get implements Client.
func (c *BifrostClient) Get(ctx context.Context, id string) (*VirtualKey, error) {
	var resp struct {
		VirtualKey bifrostKey `json:"virtual_key"`
	}
	if err := c.do(ctx, http.MethodGet, virtualKeysPath+"/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.VirtualKey.toVirtualKey(), nil
}

// FindByName implements Client.
func (c *BifrostClient) FindByName(ctx context.Context, name string) (*VirtualKey, error) {
	var resp struct {
		VirtualKeys []bifrostKey `json:"virtual_keys"`
	}
	if err := c.do(ctx, http.MethodGet, virtualKeysPath, nil, &resp); err != nil {
		return nil, err
	}
	for _, k := range resp.VirtualKeys {
		if k.Name == name {
			return k.toVirtualKey(), nil
		}
	}
	return nil, ErrNotFound
}

// Create implements Client.
func (c *BifrostClient) Create(ctx context.Context, spec VirtualKeySpec) (*VirtualKey, error) {
	var resp struct {
		VirtualKey bifrostKey `json:"virtual_key"`
	}
	if err := c.do(ctx, http.MethodPost, virtualKeysPath, toWriteRequest(spec, true), &resp); err != nil {
		return nil, err
	}
	key := resp.VirtualKey.toVirtualKey()
	if key.ID == "" || key.Value == "" {
		return nil, fmt.Errorf("bifrost: create %q returned no id or value", spec.Name)
	}
	return key, nil
}

// Update implements Client.
func (c *BifrostClient) Update(ctx context.Context, id string, spec VirtualKeySpec) error {
	return c.do(ctx, http.MethodPut, virtualKeysPath+"/"+url.PathEscape(id), toWriteRequest(spec, false), nil)
}

// Delete implements Client.
func (c *BifrostClient) Delete(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, virtualKeysPath+"/"+url.PathEscape(id), nil, nil)
	if err == ErrNotFound {
		return nil
	}
	return err
}

// do sends a request and decodes a JSON response into out (if non-nil).
func (c *BifrostClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("bifrost: encoding request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("bifrost: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bifrost %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		return ErrConflict
	case resp.StatusCode >= 300:
		// Error bodies are short messages; never echo request bodies (they may hold keys).
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("bifrost %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("bifrost %s %s: decoding response: %w", method, path, err)
	}
	return nil
}
