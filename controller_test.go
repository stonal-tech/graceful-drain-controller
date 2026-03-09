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

func createPod(t *testing.T, ctx context.Context, cl client.Client, namespace, name, nodeName string, replicaSet *appsv1.ReplicaSet) {
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
			NodeName: nodeName,
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

// --- Reconciler Tests ---

func TestReconcilerRemovesAnnotationOnComplete(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-complete-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "complete-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Format(time.RFC3339),
	})

	// Simulate completed rollout.
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.UnavailableReplicas = 0
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

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	// Verify tracking annotation was removed.
	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if _, ok := updated.Annotations[AnnotationDrainRestartedAt]; ok {
		t.Error("expected tracking annotation to be removed after rollout complete")
	}
}

func TestReconcilerRequeuesWhileInProgress(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-requeue-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	deploy := createDeployment(t, ctx, te.client, ns, "rolling-app", 1, map[string]string{
		AnnotationDrainRestartedAt: time.Now().Format(time.RFC3339),
	})

	// Simulate incomplete rollout (ReadyReplicas=0).
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

	// Verify tracking annotation was removed.
	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if _, ok := updated.Annotations[AnnotationDrainRestartedAt]; ok {
		t.Error("expected tracking annotation to be removed after timeout")
	}
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
