package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type testEnv struct {
	env      *envtest.Environment
	client   client.Client
	recorder *events.FakeRecorder
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	// Start cache in the background.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		if err := mgr.GetCache().Start(ctx); err != nil {
			t.Errorf("start cache: %v", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache sync failed")
	}

	return &testEnv{
		env:      env,
		client:   mgr.GetClient(),
		recorder: events.NewFakeRecorder(100),
	}
}

func newReconciler(te *testEnv, opts ...func(*DeploymentReconciler)) *DeploymentReconciler {
	reconciler := &DeploymentReconciler{
		Client:          te.client,
		Recorder:        te.recorder,
		RequeueInterval: 5 * time.Second,
		RolloutTimeout:  5 * time.Minute,
	}

	for _, o := range opts {
		o(reconciler)
	}

	return reconciler
}

func int32Ptr(val int32) *int32 { return &val }

func createDeployment(
	t *testing.T, ctx context.Context, cl client.Client,
	namespace, name string, replicas int32, annotations map[string]string,
) *appsv1.Deployment {
	t.Helper()

	maxSurge := intstr.FromInt32(1)
	maxUnavail := intstr.FromInt32(0)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavail,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "busybox",
					}},
				},
			},
		},
	}

	if err := cl.Create(ctx, deploy); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	return deploy
}

func createReplicaSet(t *testing.T, ctx context.Context, cl client.Client, deploy *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()

	isController := true
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name + "-abc123",
			Namespace: deploy.Namespace,
			Labels:    map[string]string{"app": deploy.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploy.Name,
				UID:        deploy.UID,
				Controller: &isController,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: deploy.Spec.Replicas,
			Selector: deploy.Spec.Selector,
			Template: deploy.Spec.Template,
		},
	}

	if err := cl.Create(ctx, replicaSet); err != nil {
		t.Fatalf("create replicaset: %v", err)
	}

	return replicaSet
}

func createPod(t *testing.T, ctx context.Context, cl client.Client, namespace, name string, replicaSet *appsv1.ReplicaSet) {
	t.Helper()

	isController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": replicaSet.Labels["app"]},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       replicaSet.Name,
				UID:        replicaSet.UID,
				Controller: &isController,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "busybox",
			}},
		},
	}

	if err := cl.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Update status to Running.
	pod.Status.Phase = corev1.PodRunning
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}

func createNamespace(t *testing.T, ctx context.Context, cl client.Client, name string) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
}

// waitForStatusSync polls until the cached client reflects the expected status.
// This is needed because envtest uses a cached client — Status().Update() writes
// to the API server, but subsequent Get() calls read from the cache which may lag.
func waitForStatusSync(t *testing.T, ctx context.Context, cl client.Client, name, namespace string, readyReplicas int32) {
	t.Helper()

	for range 20 {
		var deploy appsv1.Deployment
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
			t.Fatalf("get deployment: %v", err)
		}

		if deploy.Status.ReadyReplicas == readyReplicas {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("timed out waiting for status to sync in cache")
}

// waitForAnnotationRemoved polls until the tracking annotation is removed from the deployment.
// This is needed because envtest uses a cached client that may return stale data briefly.
func waitForAnnotationRemoved(t *testing.T, ctx context.Context, cl client.Client, name, namespace string) {
	t.Helper()

	for range 20 {
		var deploy appsv1.Deployment
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
			t.Fatalf("get deployment: %v", err)
		}

		if _, ok := deploy.Annotations[AnnotationDrainRestartedAt]; !ok {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Error("expected tracking annotation to be removed")
}

// waitForPodTemplateAnnotation polls until the cached client reflects the restart
// annotation the reconciler patched onto the pod template. Needed because envtest
// uses a cached client that may return stale data briefly.
func waitForPodTemplateAnnotation(t *testing.T, ctx context.Context, cl client.Client, name, namespace, key string) {
	t.Helper()

	for range 20 {
		var deploy appsv1.Deployment
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
			t.Fatalf("get deployment: %v", err)
		}

		if _, ok := deploy.Spec.Template.Annotations[key]; ok {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Errorf("timed out waiting for pod template annotation %s", key)
}

// --- Reconciler Tests ---

func TestReconcilerRemovesAnnotationOnComplete(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-complete-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	now := time.Now().Format(time.RFC3339)
	deploy := createDeployment(t, ctx, te.client, ns, "complete-app", 1, map[string]string{
		AnnotationDrainRestartedAt: now,
	})

	// Set pod template annotation so the reconciler knows the restart was already triggered.
	// This bumps the generation, so we must re-fetch and update status after.
	specPatch := client.MergeFrom(deploy.DeepCopy())
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations[AnnotationRestartedAt] = now
	if err := te.client.Patch(ctx, deploy, specPatch); err != nil {
		t.Fatalf("patch deployment: %v", err)
	}

	// Re-fetch to get the current generation after the spec patch.
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	// Simulate completed rollout with the current generation.
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.UnavailableReplicas = 0
	if err := te.client.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	// Wait for the cached client to reflect the status update.
	waitForStatusSync(t, ctx, te.client, deploy.Name, ns, 1)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: deploy.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	waitForAnnotationRemoved(t, ctx, te.client, deploy.Name, ns)
}

func TestReconcilerTriggersRolloutRestart(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-trigger-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	// Create deployment with only the tracking annotation (as the webhook would set it).
	deploy := createDeployment(t, ctx, te.client, ns, "trigger-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Format(time.RFC3339),
	})

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: deploy.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}

	// Verify the pod template annotation was set by the reconciler.
	waitForPodTemplateAnnotation(t, ctx, te.client, deploy.Name, ns, AnnotationRestartedAt)
}

func TestReconcilerRequeuesWhileInProgress(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-requeue-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	now := time.Now().Format(time.RFC3339)
	deploy := createDeployment(t, ctx, te.client, ns, "rolling-app", 1, map[string]string{
		AnnotationDrainRestartedAt: now,
	})

	// Set pod template annotation so the reconciler knows the restart was already triggered.
	patch := client.MergeFrom(deploy.DeepCopy())
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations[AnnotationRestartedAt] = now
	if err := te.client.Patch(ctx, deploy, patch); err != nil {
		t.Fatalf("patch deployment: %v", err)
	}

	// Re-fetch and simulate incomplete rollout (ReadyReplicas=0).
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 0
	deploy.Status.ReadyReplicas = 0
	deploy.Status.UnavailableReplicas = 1
	if err := te.client.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: deploy.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}
}

func TestReconcilerHandlesTimeout(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-timeout-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "timeout-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
	})

	r := newReconciler(te, func(r *DeploymentReconciler) {
		r.RolloutTimeout = 5 * time.Minute
	})
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: deploy.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after timeout, got %v", result.RequeueAfter)
	}

	waitForAnnotationRemoved(t, ctx, te.client, deploy.Name, ns)
}

func TestReconcilerNoopWithoutAnnotation(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-noop-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "plain-app", 1, nil)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: deploy.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for deployment without tracking annotation, got %v", result.RequeueAfter)
	}
}
