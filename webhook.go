package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	errRolloutInProgress = errors.New("graceful drain: rollout restart in progress, retry later")
	errRolloutTriggered  = errors.New("graceful drain: triggered rollout restart, retry later")
)

// EvictionHandler is a ValidatingAdmissionWebhook that intercepts pod evictions.
// It blocks eviction of singleton Deployment pods until a rollout restart completes.
type EvictionHandler struct {
	client.Client
	EnabledAnnotation string
	RolloutTimeout    time.Duration
}

// Handle processes an admission request for a pod eviction.
func (h *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Don't trigger side effects on dry-run requests.
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("dry-run")
	}

	// Decode the Eviction object to get the target pod name.
	var eviction policyv1.Eviction
	if err := json.Unmarshal(req.Object.Raw, &eviction); err != nil {
		slog.WarnContext(ctx, "failed to decode eviction object", "error", err)
		return admission.Allowed("failed to decode eviction, allowing")
	}

	podName := eviction.Name
	podNamespace := req.Namespace

	if podName == "" {
		podName = req.Name
	}

	if podNamespace == "" {
		podNamespace = eviction.Namespace
	}

	// Fetch the target pod.
	var pod corev1.Pod
	if err := h.Get(ctx, client.ObjectKey{Name: podName, Namespace: podNamespace}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			// Pod is already gone — allow eviction.
			return admission.Allowed("pod not found, already deleted")
		}
		slog.WarnContext(ctx, "failed to get pod, denying eviction to be safe",
			"pod", podName, "namespace", podNamespace, "error", err)
		return admission.Errored(http.StatusTooManyRequests,
			fmt.Errorf("graceful drain: cache not ready, retry later: %w", err))
	}

	// Resolve owning Deployment (pod → ReplicaSet → Deployment).
	deploy, err := getOwningDeployment(ctx, h.Client, &pod)
	if err != nil {
		slog.WarnContext(ctx, "failed to resolve owning deployment, allowing eviction",
			"pod", podName, "namespace", podNamespace, "error", err)
		return admission.Allowed("failed to resolve deployment, allowing eviction")
	}

	if deploy == nil {
		return admission.Allowed("pod has no owning deployment")
	}

	// Check eligibility.
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		return admission.Allowed("deployment has replicas != 1")
	}

	if h.EnabledAnnotation != "" {
		if deploy.Annotations == nil || deploy.Annotations[h.EnabledAnnotation] != "true" {
			return admission.Allowed("deployment does not have enabled annotation")
		}
	}

	if deploy.DeletionTimestamp != nil {
		return admission.Allowed("deployment is being deleted")
	}

	// Check rollout state.
	restartedAtStr, hasTracking := deploy.Annotations[AnnotationDrainRestartedAt]

	// Case: deployment already has 2+ Ready replicas → rollout done, eviction safe.
	if deploy.Status.ReadyReplicas >= 2 {
		slog.InfoContext(ctx, "rollout complete, allowing eviction",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
		return admission.Allowed("rollout complete, eviction safe")
	}

	if hasTracking {
		// Check if timed out.
		restartedAt, err := time.Parse(time.RFC3339, restartedAtStr)
		if err == nil && time.Since(restartedAt) > h.RolloutTimeout {
			slog.WarnContext(ctx, "rollout timeout exceeded, allowing eviction",
				"deployment", deploy.Name, "namespace", deploy.Namespace)
			return admission.Allowed("rollout timeout exceeded, allowing eviction")
		}

		// Rollout in progress → deny.
		slog.InfoContext(ctx, "rollout in progress, denying eviction",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
		return admission.Errored(http.StatusTooManyRequests,
			fmt.Errorf("%w: deployment %s/%s", errRolloutInProgress, deploy.Namespace, deploy.Name))
	}

	// No tracking annotation → request rollout restart (reconciler will trigger the actual rollout).
	if err := requestRolloutRestart(ctx, h.Client, deploy); err != nil {
		slog.ErrorContext(ctx, "failed to request rollout restart, allowing eviction",
			"deployment", deploy.Name, "namespace", deploy.Namespace, "error", err)
		return admission.Allowed("failed to request rollout restart, allowing eviction")
	}

	slog.InfoContext(ctx, "requested rollout restart, denying eviction",
		"deployment", deploy.Name, "namespace", deploy.Namespace,
		"pod", podName)

	return admission.Errored(http.StatusTooManyRequests,
		fmt.Errorf("%w: deployment %s/%s", errRolloutTriggered, deploy.Namespace, deploy.Name))
}
