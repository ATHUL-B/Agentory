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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testUser = "admin"
	testPass = "s3cret"
	testID   = "vk-123"
	testName = "agentory:team/a"
)

// fakeBifrost mimics the subset of Bifrost's governance API the client uses.
type fakeBifrost struct {
	t        *testing.T
	lastBody map[string]any
	status   int    // forced status for the next request, if non-zero
	body     string // forced body for the next request
}

func (f *fakeBifrost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != testUser || p != testPass {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.lastBody = nil
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &f.lastBody); err != nil {
				f.t.Errorf("request body is not JSON: %v", err)
			}
		}
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == virtualKeysPath:
		_, _ = io.WriteString(w, `{"message":"ok","virtual_key":{"id":"vk-123","name":"agentory:team/a","value":"sk-bf-abc"}}`)
	case r.Method == http.MethodGet && r.URL.Path == virtualKeysPath+"/vk-123":
		// SecretVar object form of the value.
		_, _ = io.WriteString(w, `{"virtual_key":{"id":"vk-123","name":"agentory:team/a","value":{"value":"sk-bf-abc","type":"plain"}}}`)
	case r.Method == http.MethodGet && r.URL.Path == virtualKeysPath:
		_, _ = io.WriteString(w, `{"virtual_keys":[{"id":"vk-1","name":"other"},{"id":"vk-123","name":"agentory:team/a"}]}`)
	case r.Method == http.MethodPut && r.URL.Path == virtualKeysPath+"/vk-123":
		_, _ = io.WriteString(w, `{"message":"updated"}`)
	case r.Method == http.MethodDelete && r.URL.Path == virtualKeysPath+"/vk-123":
		_, _ = io.WriteString(w, `{"message":"deleted"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Virtual key not found"}`)
	}
}

func newTestClient(t *testing.T) (*BifrostClient, *fakeBifrost) {
	t.Helper()
	fb := &fakeBifrost{t: t}
	srv := httptest.NewServer(fb)
	t.Cleanup(srv.Close)
	return NewBifrostClient(srv.URL+"/", testUser, testPass, srv.Client()), fb
}

var spec = VirtualKeySpec{
	Name: testName, Description: "d", Provider: "mock", Model: "mock-model",
	Metadata: map[string]string{"agentory.agent": "a"},
}

func TestCreateSendsLeastPrivilegeRequest(t *testing.T) {
	c, fb := newTestClient(t)
	key, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if *key != (VirtualKey{ID: testID, Name: testName, Value: "sk-bf-abc"}) {
		t.Errorf("key = %+v", key)
	}
	if fb.lastBody["name"] != testName || fb.lastBody["is_active"] != true {
		t.Errorf("body = %v", fb.lastBody)
	}
	pcs, _ := fb.lastBody["provider_configs"].([]any)
	if len(pcs) != 1 {
		t.Fatalf("provider_configs = %v, want exactly one", fb.lastBody["provider_configs"])
	}
	pc := pcs[0].(map[string]any)
	if pc["provider"] != "mock" || len(pc["allowed_models"].([]any)) != 1 || pc["allowed_models"].([]any)[0] != "mock-model" {
		t.Errorf("provider config = %v, want only mock/mock-model", pc)
	}
	if fb.lastBody["metadata"].(map[string]any)["agentory.agent"] != "a" {
		t.Errorf("metadata = %v", fb.lastBody["metadata"])
	}
}

func TestGetAcceptsObjectFormValue(t *testing.T) {
	c, _ := newTestClient(t)
	key, err := c.Get(context.Background(), testID)
	if err != nil || key.Value != "sk-bf-abc" {
		t.Fatalf("Get = %+v, %v", key, err)
	}
	if _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) err = %v, want ErrNotFound", err)
	}
}

func TestFindByName(t *testing.T) {
	c, _ := newTestClient(t)
	key, err := c.FindByName(context.Background(), testName)
	if err != nil || key.ID != testID {
		t.Fatalf("FindByName = %+v, %v", key, err)
	}
	if _, err := c.FindByName(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindByName(nope) err = %v, want ErrNotFound", err)
	}
}

func TestUpdateOmitsName(t *testing.T) {
	c, fb := newTestClient(t)
	if err := c.Update(context.Background(), testID, spec); err != nil {
		t.Fatal(err)
	}
	if _, ok := fb.lastBody["name"]; ok {
		t.Errorf("update must not rename the key: %v", fb.lastBody)
	}
	if fb.lastBody["provider_configs"] == nil {
		t.Error("update must send provider_configs")
	}
}

func TestDeleteIgnoresMissingKeys(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.Delete(context.Background(), testID); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if err := c.Delete(context.Background(), "already-gone"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
}

func TestErrorMapping(t *testing.T) {
	c, fb := newTestClient(t)

	fb.status, fb.body = http.StatusConflict, `{"error":"A virtual key with this name already exists"}`
	if _, err := c.Create(context.Background(), spec); !errors.Is(err, ErrConflict) {
		t.Errorf("409 err = %v, want ErrConflict", err)
	}

	fb.status, fb.body = http.StatusInternalServerError, strings.Repeat("x", 2000)
	_, err := c.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || len(err.Error()) > 700 {
		t.Errorf("500 err = %v (len %d), want short HTTP 500 error", err, len(err.Error()))
	}

	fb.status, fb.body = http.StatusOK, `{"virtual_key":{"id":""}}`
	if _, err := c.Create(context.Background(), spec); err == nil {
		t.Error("create without id/value must fail")
	}
}

func TestWrongCredentialsFail(t *testing.T) {
	fb := &fakeBifrost{t: t}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	c := NewBifrostClient(srv.URL, testUser, "wrong", srv.Client())
	if _, err := c.Get(context.Background(), testID); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("err = %v, want HTTP 401", err)
	}
}
