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
	"errors"
	"regexp"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

func TestSpecRevision(t *testing.T) {
	spec := newDeclarativeAgent("a").Spec

	first, err := SpecRevision("payments-oncall", &spec)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^payments-oncall-[0-9a-f]{10}$`).MatchString(first) {
		t.Fatalf("revision %q does not match <name>-<10 hex chars>", first)
	}

	again, _ := SpecRevision("payments-oncall", &spec)
	if again != first {
		t.Fatalf("same spec gave different revisions: %q vs %q", first, again)
	}

	changed := spec.DeepCopy()
	changed.Model.Name = "another-model"
	other, _ := SpecRevision("payments-oncall", changed)
	if other == first {
		t.Fatalf("different specs gave the same revision %q", first)
	}
}

func TestTaskPhaseIsTerminal(t *testing.T) {
	terminal := map[agentoryv1alpha1.TaskPhase]bool{
		agentoryv1alpha1.TaskPhasePending:       false,
		agentoryv1alpha1.TaskPhaseRunning:       false,
		agentoryv1alpha1.TaskPhaseAwaitingInput: false,
		agentoryv1alpha1.TaskPhaseSucceeded:     true,
		agentoryv1alpha1.TaskPhaseFailed:        true,
		agentoryv1alpha1.TaskPhaseCancelled:     true,
		"":                                      false,
	}
	for phase, want := range terminal {
		if got := phase.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", phase, got, want)
		}
	}
}

func TestStatusUpdateResult(t *testing.T) {
	gr := schema.GroupResource{Group: "agentory.dev", Resource: "agents"}
	if res, err := statusUpdateResult(apierrors.NewConflict(gr, "a", errors.New("stale"))); err != nil || res.RequeueAfter != conflictRequeueDelay {
		t.Errorf("conflict: got %+v, %v; want quiet requeue", res, err)
	}
	if res, err := statusUpdateResult(apierrors.NewNotFound(gr, "a")); err != nil || res.RequeueAfter != 0 {
		t.Errorf("not found: got %+v, %v; want done", res, err)
	}
	if _, err := statusUpdateResult(errors.New("boom")); err == nil {
		t.Error("other errors must be returned")
	}
}
