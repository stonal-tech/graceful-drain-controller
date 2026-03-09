package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func makeEvictionRequest(t *testing.T, podName, namespace string, dryRun bool) admission.Request {
	t.Helper()

	eviction := policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
	}

	raw, err := json.Marshal(eviction)
	if err != nil {
		t.Fatalf("marshal eviction: %v", err)
	}

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Name:      podName,
			Namespace: namespace,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	if dryRun {
		req.DryRun = &dryRun
	}

	return req
}

func TestWebhookAllowDryRun(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "some-pod", "default", true))
	if !resp.Allowed {
		t.Errorf("expected dry-run to be allowed, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookAllowNonDeploymentPod(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-noowner-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	// Create a bare pod with no owner.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bare-pod", Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.client.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "bare-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected bare pod eviction to be allowed, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookAllowReplicasGreaterThanOne(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-multi-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "multi-app", 3, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "multi-app-pod", "", rs)

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "multi-app-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected replicas>1 eviction to be allowed, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookAllowDeploymentBeingDeleted(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-deleting-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "deleting-app", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "deleting-app-pod", "", rs)

	// Mark deployment as being deleted.
	if err := te.client.Delete(ctx, deploy); err != nil {
		t.Fatalf("delete deployment: %v", err)
	}

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "deleting-app-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected eviction of deleting deployment's pod to be allowed, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookAllowNotMatchingAnnotation(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-noannot-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "no-annot-app", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "no-annot-app-pod", "", rs)

	h := &EvictionHandler{
		Client:            te.client,
		EnabledAnnotation: "graceful-drain.stonal.com/enabled",
		RolloutTimeout:    5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "no-annot-app-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected non-annotated deployment eviction to be allowed, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookDenyAndTriggerRestart(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-deny-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "singleton-app", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "singleton-app-pod", "", rs)

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "singleton-app-pod", ns, false))
	if resp.Allowed {
		t.Error("expected eviction to be denied (429)")
	}

	if resp.Result.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 status, got %d", resp.Result.Code)
	}

	// Verify deployment got both annotations.
	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if _, ok := updated.Annotations[AnnotationDrainRestartedAt]; !ok {
		t.Error("expected tracking annotation on deployment metadata")
	}

	if _, ok := updated.Spec.Template.Annotations[AnnotationRestartedAt]; !ok {
		t.Error("expected restartedAt annotation on pod template")
	}
}

func TestWebhookDenyWhileRolloutInProgress(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-inprog-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "rolling-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Format(time.RFC3339),
	})
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "rolling-app-pod", "", rs)

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "rolling-app-pod", ns, false))
	if resp.Allowed {
		t.Error("expected eviction to be denied while rollout in progress")
	}

	if resp.Result.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 status, got %d", resp.Result.Code)
	}
}

func TestWebhookAllowAfterTimeout(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-timeout-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "timeout-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
	})
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "timeout-app-pod", "", rs)

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "timeout-app-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected eviction to be allowed after timeout, got denied: %s", resp.Result.Message)
	}
}

func TestWebhookAllowAfterRolloutComplete(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-complete-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "complete-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Format(time.RFC3339),
	})

	// Simulate surge: 2 replicas running (1 old + 1 new), both ready.
	deploy.Status.Replicas = 2
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.ReadyReplicas = 2
	if err := te.client.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "complete-app-pod", "", rs)

	h := &EvictionHandler{
		Client:         te.client,
		RolloutTimeout: 5 * time.Minute,
	}

	resp := h.Handle(ctx, makeEvictionRequest(t, "complete-app-pod", ns, false))
	if !resp.Allowed {
		t.Errorf("expected eviction to be allowed after rollout complete, got denied: %s", resp.Result.Message)
	}
}
