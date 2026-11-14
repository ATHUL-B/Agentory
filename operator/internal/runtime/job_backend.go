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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentoryv1alpha1 "github.com/ATHUL-B/Agentory/operator/api/v1alpha1"
	"github.com/ATHUL-B/Agentory/operator/internal/ownership"
)

// Labels on Task workloads.
const (
	LabelTask = "agentory.dev/task"
	// jobNameLabel is set by the Job controller on the pods it creates.
	jobNameLabel = "batch.kubernetes.io/job-name"
)

// Reasons reported by JobBackend.
const (
	ReasonReadyForTasks  = "ReadyForTasks"
	ReasonJobPending     = "JobPending"
	ReasonJobRunning     = "JobRunning"
	ReasonHarnessSucceed = "HarnessSucceeded"
	ReasonHarnessFailed  = "HarnessFailed"
	ReasonTimeout        = "Timeout"
	ReasonImageProblem   = "ImageProblem"
)

const (
	harnessContainer   = "harness"
	harnessUID         = int64(1000)
	jobNameMax         = 52 // pods append a suffix; keep the job-name label valid
	jobTTLSeconds      = int32(3600)
	workspaceVolume    = "workspace"
	tmpVolume          = "tmp"
	workspaceMountPath = "/workspace"
)

// Task environment variables, on top of the platform environment.
const (
	EnvTask        = "AGENTORY_TASK"
	EnvTaskMessage = "AGENTORY_TASK_MESSAGE"
	EnvTaskData    = "AGENTORY_TASK_DATA"
)

// TaskBackend runs Tasks for the Agents it supports.
type TaskBackend interface {
	// EnsureTask starts the Task's workload if it isn't running yet and reports its state.
	// Repeated calls never start a second workload.
	EnsureTask(ctx context.Context, agent *agentoryv1alpha1.Agent, task *agentoryv1alpha1.Task, deps Deps) (TaskRuntimeStatus, error)
}

// TaskBackendFor returns the Agent's backend if it can run Tasks.
func (r *Registry) TaskBackendFor(agent *agentoryv1alpha1.Agent) (TaskBackend, bool) {
	b, ok := r.For(agent)
	if !ok {
		return nil, false
	}
	tb, ok := b.(TaskBackend)
	return tb, ok
}

// TaskRuntimeStatus is what a backend reports about a Task's workload.
type TaskRuntimeStatus struct {
	Phase       agentoryv1alpha1.TaskPhase
	Reason      string
	Message     string
	Workload    string // e.g. the Job name
	StartedAt   *metav1.Time
	CompletedAt *metav1.Time
	// Result is set once the harness has reported (success or failure).
	Result *HarnessResult
}

// HarnessResult is the JSON a harness writes to its termination message
// (harness/claude-code/runner.py, TaskResult.to_json).
type HarnessResult struct {
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	TraceID  string `json:"traceId"`
	NumTurns int    `json:"numTurns"`
	Usage    struct {
		InputTokens  int64  `json:"inputTokens"`
		OutputTokens int64  `json:"outputTokens"`
		CostUSD      string `json:"costUsd"`
	} `json:"usage"`
	Artifacts []agentoryv1alpha1.TaskArtifact `json:"artifacts"`
}

// JobBackend runs harness agents (runtime "harness", lifecycle "perTask"): one
// Kubernetes Job per Task, owned by the Task.
type JobBackend struct {
	Client client.Client
	Scheme *k8sruntime.Scheme
	Env    EnvBuilder
	Policy PolicyBuilder
	// Images is the default image per harness kind; spec.harness.image overrides it.
	Images map[agentoryv1alpha1.HarnessKind]string
}

// NewJobBackend wires a JobBackend to the given gateways and harness images.
func NewJobBackend(c client.Client, scheme *k8sruntime.Scheme, cfg GatewayConfig,
	images map[agentoryv1alpha1.HarnessKind]string) *JobBackend {
	return &JobBackend{Client: c, Scheme: scheme, Env: EnvBuilder{Config: cfg}, Policy: PolicyBuilder{Config: cfg}, Images: images}
}

// Name implements Backend.
func (b *JobBackend) Name() string { return "job" }

// Supports implements Backend: harness agents whose kind has an image.
func (b *JobBackend) Supports(agent *agentoryv1alpha1.Agent) bool {
	return agent.Spec.Runtime == agentoryv1alpha1.RuntimeHarness &&
		agent.Spec.Lifecycle == agentoryv1alpha1.LifecyclePerTask &&
		agent.Spec.Harness != nil && b.image(agent) != ""
}

func (b *JobBackend) image(agent *agentoryv1alpha1.Agent) string {
	if agent.Spec.Harness.Image != "" {
		return agent.Spec.Harness.Image
	}
	return b.Images[agent.Spec.Harness.Kind]
}

// EnsureAgent implements Backend. A per-task agent has no long-lived workload:
// it gets the identity and network lockdown its Task pods use.
func (b *JobBackend) EnsureAgent(ctx context.Context, agent *agentoryv1alpha1.Agent, _ Deps) (AgentRuntimeStatus, error) {
	meta := func() metav1.ObjectMeta { return metav1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace} }
	sa := &corev1.ServiceAccount{ObjectMeta: meta()}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: meta()}
	labels := ObjectLabels(agent)
	for _, step := range []struct {
		obj    client.Object
		mutate func()
	}{
		{sa, func() { sa.AutomountServiceAccountToken = new(false) }},
		// No serving port: Task pods accept no ingress at all.
		{policy, func() { policy.Spec = b.Policy.Build(agent, 0) }},
	} {
		if err := ownership.Ensure(ctx, b.Client, b.Scheme, agent, step.obj, labels, step.mutate); err != nil {
			if errors.Is(err, ownership.ErrNotOwned) {
				return AgentRuntimeStatus{Reason: ReasonResourceConflict, Message: err.Error()}, nil
			}
			return AgentRuntimeStatus{}, err
		}
	}
	msg := fmt.Sprintf("runs one Job per Task with image %s", b.image(agent))
	if agent.Spec.Harness.Workspace != nil {
		// Cloning needs a controlled egress exception for the git host (backlog 7a).
		msg += "; harness.workspace is not supported yet, so Tasks start in an empty workspace"
	}
	return AgentRuntimeStatus{WorkloadReady: true, Reason: ReasonReadyForTasks, Message: msg}, nil
}

// JobName is the Job for a Task: the Task name, shortened with a hash if needed.
func JobName(task *agentoryv1alpha1.Task) string {
	if len(task.Name) <= jobNameMax {
		return task.Name
	}
	sum := sha256.Sum256([]byte(task.Name))
	return strings.TrimRight(task.Name[:jobNameMax-7], "-.") + "-" + hex.EncodeToString(sum[:])[:6]
}

// EnsureTask implements TaskBackend.
func (b *JobBackend) EnsureTask(ctx context.Context, agent *agentoryv1alpha1.Agent, task *agentoryv1alpha1.Task,
	deps Deps) (TaskRuntimeStatus, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: task.Namespace, Name: JobName(task)}
	switch err := b.Client.Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		job = b.newJob(agent, task, deps)
		if err := controllerutil.SetControllerReference(task, job, b.Scheme); err != nil {
			return TaskRuntimeStatus{}, err
		}
		if err := b.Client.Create(ctx, job); err != nil {
			return TaskRuntimeStatus{}, fmt.Errorf("creating job %s: %w", key.Name, err)
		}
	case err != nil:
		return TaskRuntimeStatus{}, err
	case !metav1.IsControlledBy(job, task):
		return TaskRuntimeStatus{
			Phase: agentoryv1alpha1.TaskPhaseFailed, Reason: ReasonResourceConflict,
			Message: fmt.Sprintf("Job %s/%s %v", key.Namespace, key.Name, ownership.ErrNotOwned),
		}, nil
	}
	return b.taskStatus(ctx, job)
}

func (b *JobBackend) newJob(agent *agentoryv1alpha1.Agent, task *agentoryv1alpha1.Task, deps Deps) *batchv1.Job {
	labels := ObjectLabels(agent)
	labels[LabelTask] = task.Name

	env := b.Env.Build(agent, deps.Revision)
	env = append(env,
		corev1.EnvVar{Name: EnvTask, Value: task.Name},
		corev1.EnvVar{Name: EnvTaskMessage, Value: task.Spec.Input.Message},
	)
	if task.Spec.Input.Data != nil {
		env = append(env, corev1.EnvVar{Name: EnvTaskData, Value: string(task.Spec.Input.Data.Raw)})
	}

	var resources corev1.ResourceRequirements
	if agent.Spec.Resources != nil {
		resources = *agent.Spec.Resources.DeepCopy()
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: JobName(task), Namespace: task.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// One attempt: a coding run is not safely repeatable.
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(jobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{AnnotationRevision: deps.Revision},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           agent.Name,
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						RunAsUser:      new(harnessUID),
						FSGroup:        new(harnessUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:                     harnessContainer,
						Image:                    b.image(agent),
						Env:                      env,
						Resources:                resources,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: workspaceVolume, MountPath: workspaceMountPath},
							{Name: tmpVolume, MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: workspaceVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	if task.Spec.Timeout != nil {
		job.Spec.ActiveDeadlineSeconds = new(int64(task.Spec.Timeout.Seconds()))
	}
	return job
}

// taskStatus maps the Job (and its pod's termination message) to a Task status.
func (b *JobBackend) taskStatus(ctx context.Context, job *batchv1.Job) (TaskRuntimeStatus, error) {
	st := TaskRuntimeStatus{Workload: job.Name, StartedAt: job.Status.StartTime}
	pod, err := b.harnessPod(ctx, job)
	if err != nil {
		return TaskRuntimeStatus{}, err
	}
	result, raw := terminationResult(pod)
	st.Result = result

	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			st.Phase, st.Reason = agentoryv1alpha1.TaskPhaseSucceeded, ReasonHarnessSucceed
			st.CompletedAt = job.Status.CompletionTime
			st.Message = summaryOr(result, raw, "harness completed")
			return st, nil
		case batchv1.JobFailed:
			st.Phase, st.Reason = agentoryv1alpha1.TaskPhaseFailed, ReasonHarnessFailed
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				st.Reason = ReasonTimeout
			}
			completed := c.LastTransitionTime
			st.CompletedAt = &completed
			st.Message = summaryOr(result, raw, c.Message)
			return st, nil
		}
	}

	if reason, msg := podProblem(pod); reason != "" {
		st.Phase, st.Reason, st.Message = agentoryv1alpha1.TaskPhasePending, reason, msg
		return st, nil
	}
	if job.Status.Active > 0 && pod != nil && pod.Status.Phase == corev1.PodRunning {
		st.Phase, st.Reason, st.Message = agentoryv1alpha1.TaskPhaseRunning, ReasonJobRunning, "harness running in job "+job.Name
		return st, nil
	}
	st.Phase, st.Reason, st.Message = agentoryv1alpha1.TaskPhasePending, ReasonJobPending, "waiting for job "+job.Name+" to start"
	return st, nil
}

// harnessPod returns the Job's pod, if any (one attempt, so at most one).
func (b *JobBackend) harnessPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := b.Client.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{jobNameLabel: job.Name}); err != nil {
		return nil, fmt.Errorf("listing pods of job %s: %w", job.Name, err)
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	return newest, nil
}

// terminationResult parses the harness container's termination message.
func terminationResult(pod *corev1.Pod) (*HarnessResult, string) {
	if pod == nil {
		return nil, ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != harnessContainer || cs.State.Terminated == nil {
			continue
		}
		raw := strings.TrimSpace(cs.State.Terminated.Message)
		var r HarnessResult
		if raw != "" && json.Unmarshal([]byte(raw), &r) == nil && r.Status != "" {
			return &r, raw
		}
		return nil, raw
	}
	return nil, ""
}

// podProblem reports a pod that cannot start (bad image, missing config).
func podProblem(pod *corev1.Pod) (string, string) {
	if pod == nil {
		return "", ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return ReasonImageProblem, fmt.Sprintf("%s: %s", w.Reason, w.Message)
			case "CreateContainerConfigError":
				return w.Reason, w.Message
			}
		}
	}
	return "", ""
}

func summaryOr(r *HarnessResult, raw, fallback string) string {
	switch {
	case r != nil && r.Summary != "":
		return r.Summary
	case raw != "":
		return raw
	default:
		return fallback
	}
}
