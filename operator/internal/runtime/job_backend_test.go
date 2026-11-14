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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
)

const (
	testTask         = "fix-flaky-test"
	testHarnessAgent = "coder"
	testRevision     = "coder-r1"
	defaultImage     = "agentory-harness-claude-code:dev"
)

func harnessAgent(mutate ...func(*agentoryv1alpha1.Agent)) *agentoryv1alpha1.Agent {
	a := &agentoryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testHarnessAgent, Namespace: testNamespace, UID: "uid-agent"},
		Spec: agentoryv1alpha1.AgentSpec{
			Runtime:   agentoryv1alpha1.RuntimeHarness,
			Lifecycle: agentoryv1alpha1.LifecyclePerTask,
			Harness:   &agentoryv1alpha1.HarnessSpec{Kind: agentoryv1alpha1.HarnessKindClaudeCode},
			Model:     agentoryv1alpha1.ModelSpec{Provider: "mock", Name: "mock-model"},
		},
	}
	for _, m := range mutate {
		m(a)
	}
	return a
}

func task(name string) *agentoryv1alpha1.Task {
	return &agentoryv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "uid-task"},
		Spec: agentoryv1alpha1.TaskSpec{
			AgentRef: agentoryv1alpha1.AgentReference{Name: testHarnessAgent},
			Input: agentoryv1alpha1.TaskInput{
				Message: "Fix the flaky test",
				Data:    &apiextensionsv1.JSON{Raw: []byte(`{"issue":"PAY-1"}`)},
			},
			Timeout: &metav1.Duration{Duration: 10 * time.Minute},
		},
	}
}

func newJobBackend(t *testing.T, objs ...client.Object) (*JobBackend, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&batchv1.Job{}).Build()
	return NewJobBackend(c, scheme, DefaultGatewayConfig(),
		map[agentoryv1alpha1.HarnessKind]string{agentoryv1alpha1.HarnessKindClaudeCode: defaultImage}), c
}

func TestJobBackendSupports(t *testing.T) {
	b, _ := newJobBackend(t)
	if !b.Supports(harnessAgent()) {
		t.Error("claude-code harness agent must be supported")
	}
	if b.Supports(harnessAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.Harness.Kind = agentoryv1alpha1.HarnessKindCodex })) {
		t.Error("a kind without an image must not be supported")
	}
	if !b.Supports(harnessAgent(func(a *agentoryv1alpha1.Agent) {
		a.Spec.Harness.Kind = agentoryv1alpha1.HarnessKindCodex
		a.Spec.Harness.Image = "example/codex:1"
	})) {
		t.Error("spec.harness.image makes any kind runnable")
	}
	if b.Supports(containerAgent()) {
		t.Error("container agents belong to the deployment backend")
	}
}

func TestJobBackendEnsureAgentLocksDownTaskPods(t *testing.T) {
	agent := harnessAgent()
	b, c := newJobBackend(t, agent)
	st, err := b.EnsureAgent(context.Background(), agent, Deps{})
	if err != nil || !st.WorkloadReady || st.Reason != ReasonReadyForTasks || st.ServingPort != 0 {
		t.Fatalf("EnsureAgent = %+v, %v", st, err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testHarnessAgent}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 0 {
		t.Errorf("task pods must accept no ingress, got %+v", policy.Spec.Ingress)
	}
}

func TestJobBackendSaysWorkspaceIsNotSupportedYet(t *testing.T) {
	agent := harnessAgent(func(a *agentoryv1alpha1.Agent) {
		a.Spec.Harness.Workspace = &agentoryv1alpha1.WorkspaceSpec{Git: agentoryv1alpha1.GitWorkspace{URL: "https://example.com/r.git"}}
	})
	b, _ := newJobBackend(t, agent)
	st, err := b.EnsureAgent(context.Background(), agent, Deps{})
	if err != nil || !strings.Contains(st.Message, "workspace is not supported yet") {
		t.Errorf("EnsureAgent = %+v, %v; must not silently ignore harness.workspace", st, err)
	}
}

func TestEnsureTaskCreatesOneLockedDownJob(t *testing.T) {
	agent, tk := harnessAgent(), task(testTask)
	b, c := newJobBackend(t, agent, tk)
	ctx := context.Background()

	st, err := b.EnsureTask(ctx, agent, tk, Deps{Revision: testRevision})
	if err != nil || st.Phase != agentoryv1alpha1.TaskPhasePending || st.Workload != testTask {
		t.Fatalf("EnsureTask = %+v, %v", st, err)
	}
	if _, err := b.EnsureTask(ctx, agent, tk, Deps{Revision: testRevision}); err != nil {
		t.Fatal(err)
	}
	var jobs batchv1.JobList
	_ = c.List(ctx, &jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want exactly one", len(jobs.Items))
	}

	job := jobs.Items[0]
	if !metav1.IsControlledBy(&job, tk) {
		t.Error("job must be owned by the task")
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("backoff=%d deadline=%d", *job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template
	if pod.Labels[LabelAgent] != testHarnessAgent || pod.Labels[LabelTask] != testTask {
		t.Errorf("pod labels %v must select the agent's network policy and name the task", pod.Labels)
	}
	spec := pod.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever || spec.ServiceAccountName != testHarnessAgent || *spec.AutomountServiceAccountToken {
		t.Errorf("pod spec = restart:%s sa:%s", spec.RestartPolicy, spec.ServiceAccountName)
	}
	if !*spec.SecurityContext.RunAsNonRoot || *spec.SecurityContext.FSGroup != harnessUID {
		t.Errorf("security context = %+v", spec.SecurityContext)
	}
	ctr := spec.Containers[0]
	if ctr.Image != defaultImage || ctr.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("container = %s %s", ctr.Image, ctr.TerminationMessagePolicy)
	}
	env := envMap(ctr.Env)
	if env[EnvTaskMessage].Value != "Fix the flaky test" || env[EnvTaskData].Value != `{"issue":"PAY-1"}` ||
		env[EnvTask].Value != testTask || env[EnvAnthropicURL].Value == "" {
		t.Errorf("task env missing: %v", env)
	}
	if env[EnvRevision].Value != testRevision {
		t.Errorf("revision = %q", env[EnvRevision].Value)
	}
}

func TestEnsureTaskUsesImageOverride(t *testing.T) {
	agent := harnessAgent(func(a *agentoryv1alpha1.Agent) { a.Spec.Harness.Image = "example/custom:2" })
	tk := task(testTask)
	b, c := newJobBackend(t, agent, tk)
	if _, err := b.EnsureTask(context.Background(), agent, tk, Deps{}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testTask}, job)
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "example/custom:2" {
		t.Errorf("image = %q", got)
	}
}

func TestEnsureTaskRefusesForeignJob(t *testing.T) {
	agent, tk := harnessAgent(), task(testTask)
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: testNamespace}}
	b, _ := newJobBackend(t, agent, tk, foreign)
	st, err := b.EnsureTask(context.Background(), agent, tk, Deps{})
	if err != nil || st.Reason != ReasonResourceConflict || st.Phase != agentoryv1alpha1.TaskPhaseFailed {
		t.Fatalf("EnsureTask = %+v, %v", st, err)
	}
}

func TestJobName(t *testing.T) {
	if got := JobName(task("short")); got != "short" {
		t.Errorf("JobName(short) = %q", got)
	}
	long := task(strings.Repeat("a", 60) + "-x")
	got := JobName(long)
	if len(got) > jobNameMax || got == JobName(task(strings.Repeat("a", 60)+"-y")) {
		t.Errorf("JobName(long) = %q (len %d): must be short and unique", got, len(got))
	}
}

// jobWith returns the Task's Job after applying conditions, plus a pod whose
// harness container terminated with the given message.
func statusFor(t *testing.T, conds []batchv1.JobCondition, active int32, pod *corev1.Pod) TaskRuntimeStatus {
	t.Helper()
	agent, tk := harnessAgent(), task(testTask)
	objs := []client.Object{agent, tk}
	if pod != nil {
		objs = append(objs, pod)
	}
	b, c := newJobBackend(t, objs...)
	ctx := context.Background()
	if _, err := b.EnsureTask(ctx, agent, tk, Deps{}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testTask}, job)
	now := metav1.Now()
	job.Status = batchv1.JobStatus{Conditions: conds, Active: active, StartTime: &now}
	if len(conds) > 0 && conds[0].Type == batchv1.JobComplete {
		job.Status.CompletionTime = &now
	}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	st, err := b.EnsureTask(ctx, agent, tk, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func harnessPodWith(phase corev1.PodPhase, cs corev1.ContainerStatus) *corev1.Pod {
	cs.Name = harnessContainer
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testTask + "-abcde", Namespace: testNamespace,
			Labels: map[string]string{jobNameLabel: testTask}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func terminatedWith(message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}}}
}

const okResult = `{"status":"succeeded","summary":"Created hello.txt","traceId":"abc123","numTurns":2,` +
	`"usage":{"inputTokens":75,"outputTokens":15,"costUsd":"0.000750"},"artifacts":[{"type":"file","ref":"hello.txt"}]}`

func TestTaskStatusSucceededWithResult(t *testing.T) {
	st := statusFor(t, []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}, 0,
		harnessPodWith(corev1.PodSucceeded, terminatedWith(okResult)))
	if st.Phase != agentoryv1alpha1.TaskPhaseSucceeded || st.Reason != ReasonHarnessSucceed || st.CompletedAt == nil {
		t.Fatalf("status = %+v", st)
	}
	r := st.Result
	if r == nil || r.TraceID != "abc123" || r.Usage.InputTokens != 75 || r.Usage.CostUSD != "0.000750" ||
		len(r.Artifacts) != 1 || r.Artifacts[0].Ref != "hello.txt" {
		t.Errorf("result = %+v", r)
	}
	if st.Message != "Created hello.txt" {
		t.Errorf("message = %q", st.Message)
	}
}

func TestTaskStatusFailedAndTimeout(t *testing.T) {
	failed := statusFor(t, []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}, 0,
		harnessPodWith(corev1.PodFailed, terminatedWith(`{"status":"failed","summary":"model not found"}`)))
	if failed.Phase != agentoryv1alpha1.TaskPhaseFailed || failed.Reason != ReasonHarnessFailed || failed.Message != "model not found" {
		t.Errorf("failed = %+v", failed)
	}

	timeout := statusFor(t, []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job was active longer than specified deadline"}}, 0, nil)
	if timeout.Phase != agentoryv1alpha1.TaskPhaseFailed || timeout.Reason != ReasonTimeout ||
		!strings.Contains(timeout.Message, "deadline") {
		t.Errorf("timeout = %+v", timeout)
	}

	garbled := statusFor(t, []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}, 0,
		harnessPodWith(corev1.PodFailed, terminatedWith("panic: not json")))
	if garbled.Result != nil || garbled.Message != "panic: not json" {
		t.Errorf("garbled = %+v; raw message must surface", garbled)
	}
}

func TestTaskStatusRunningPendingAndImageProblems(t *testing.T) {
	running := statusFor(t, nil, 1, harnessPodWith(corev1.PodRunning, corev1.ContainerStatus{}))
	if running.Phase != agentoryv1alpha1.TaskPhaseRunning || running.StartedAt == nil {
		t.Errorf("running = %+v", running)
	}
	pending := statusFor(t, nil, 1, nil)
	if pending.Phase != agentoryv1alpha1.TaskPhasePending || pending.Reason != ReasonJobPending {
		t.Errorf("pending = %+v", pending)
	}
	pull := statusFor(t, nil, 1, harnessPodWith(corev1.PodPending, corev1.ContainerStatus{State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}}}))
	if pull.Phase != agentoryv1alpha1.TaskPhasePending || pull.Reason != ReasonImageProblem ||
		!strings.Contains(pull.Message, "pull access denied") {
		t.Errorf("image problem = %+v", pull)
	}
}

func TestRegistryTaskBackendFor(t *testing.T) {
	jb, _ := newJobBackend(t)
	reg := NewRegistry(&DeploymentBackend{}, jb)
	if _, ok := reg.TaskBackendFor(containerAgent()); ok {
		t.Error("service agents have no task backend")
	}
	if tb, ok := reg.TaskBackendFor(harnessAgent()); !ok || tb != TaskBackend(jb) {
		t.Error("harness agents must get the job backend")
	}
}
