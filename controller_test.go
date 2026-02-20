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

var defaultDrainTaints = []DrainTaint{
	{Key: "karpenter.sh/disrupted", Effect: "NoSchedule"},
	{Key: "ToBeDeletedByClusterAutoscaler", Effect: "NoSchedule"},
	{Key: "node.kubernetes.io/unschedulable", Effect: "NoSchedule"},
}

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

	// Set up field index for spec.nodeName.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create manager for indexer: %v", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		pod, ok := o.(*corev1.Pod)
		if !ok || pod.Spec.NodeName == "" {
			return nil
		}

		return []string{pod.Spec.NodeName}
	}); err != nil {
		t.Fatalf("index pods by nodeName: %v", err)
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

func newReconciler(te *testEnv, opts ...func(*NodeReconciler)) *NodeReconciler {
	reconciler := &NodeReconciler{
		Client:          te.client,
		Recorder:        te.recorder,
		DrainTaints:     defaultDrainTaints,
		RequeueInterval: 5 * time.Second,
		RolloutTimeout:  5 * time.Minute,
	}

	for _, o := range opts {
		o(reconciler)
	}

	return reconciler
}

func int32Ptr(val int32) *int32 { return &val }

func createNode(t *testing.T, ctx context.Context, cl client.Client, name string, taints []corev1.Taint) *corev1.Node {
	t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
	if err := cl.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	return node
}

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

func TestHappyPath(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-happy-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-happy-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})
	deploy := createDeployment(t, ctx, te.client, ns, "myapp", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-pod", node.Name, rs)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}

	// Verify deployment got restartedAt annotation.
	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if _, ok := updated.Spec.Template.Annotations[AnnotationRestartedAt]; !ok {
		t.Error("expected restartedAt annotation on pod template")
	}

	// Verify node got processing-since annotation.
	var updatedNode corev1.Node
	if err := te.client.Get(ctx, types.NamespacedName{Name: node.Name}, &updatedNode); err != nil {
		t.Fatalf("get node: %v", err)
	}

	if _, ok := updatedNode.Annotations[AnnotationProcessingSince]; !ok {
		t.Error("expected processing-since annotation on node")
	}
}

func TestSkipNoDrainTaint(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-notaint-%d", time.Now().UnixNano()), nil)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestSkipReplicasGreaterThanOne(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-skip-replicas-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-replicas-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})
	deploy := createDeployment(t, ctx, te.client, ns, "myapp-multi", 3, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-multi-pod", node.Name, rs)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for replicas>1, got %v", result.RequeueAfter)
	}

	// Verify deployment was NOT patched.
	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if updated.Spec.Template.Annotations != nil {
		if _, ok := updated.Spec.Template.Annotations[AnnotationRestartedAt]; ok {
			t.Error("did not expect restartedAt annotation on deployment with replicas>1")
		}
	}
}

func TestAnnotationFilterInclude(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-annot-incl-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-annot-incl-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})
	deploy := createDeployment(t, ctx, te.client, ns, "myapp-annotated", 1, map[string]string{
		"graceful-drain.stonal.com/enabled": "true",
	})
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-annotated-pod", node.Name, rs)

	r := newReconciler(te, func(r *NodeReconciler) {
		r.EnabledAnnotation = "graceful-drain.stonal.com/enabled"
	})
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}

	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if _, ok := updated.Spec.Template.Annotations[AnnotationRestartedAt]; !ok {
		t.Error("expected restartedAt annotation on annotated deployment")
	}
}

func TestAnnotationFilterExclude(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-annot-excl-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-annot-excl-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})
	deploy := createDeployment(t, ctx, te.client, ns, "myapp-noannotation", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-noannotation-pod", node.Name, rs)

	r := newReconciler(te, func(r *NodeReconciler) {
		r.EnabledAnnotation = "graceful-drain.stonal.com/enabled"
	})
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for non-annotated deployment, got %v", result.RequeueAfter)
	}

	var updated appsv1.Deployment
	if err := te.client.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if updated.Spec.Template.Annotations != nil {
		if _, ok := updated.Spec.Template.Annotations[AnnotationRestartedAt]; ok {
			t.Error("did not expect restartedAt annotation on non-annotated deployment")
		}
	}
}

func TestSkipAlreadyRestarting(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-already-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-already-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})
	deploy := createDeployment(t, ctx, te.client, ns, "myapp-restarting", 1, nil)

	// Pre-set the restartedAt annotation to simulate an in-progress restart.
	patch := client.MergeFrom(deploy.DeepCopy())
	deploy.Spec.Template.Annotations = map[string]string{
		AnnotationRestartedAt: time.Now().Format(time.RFC3339),
	}

	if err := te.client.Patch(ctx, deploy, patch); err != nil {
		t.Fatalf("patch deployment: %v", err)
	}

	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-restarting-pod", node.Name, rs)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for already-restarting deployment, got %v", result.RequeueAfter)
	}
}

func TestMultipleDeployments(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-multi-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-multi-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})

	deploy1 := createDeployment(t, ctx, te.client, ns, "app1", 1, nil)
	rs1 := createReplicaSet(t, ctx, te.client, deploy1)
	createPod(t, ctx, te.client, ns, "app1-pod", node.Name, rs1)

	deploy2 := createDeployment(t, ctx, te.client, ns, "app2", 1, nil)
	rs2 := createReplicaSet(t, ctx, te.client, deploy2)
	createPod(t, ctx, te.client, ns, "app2-pod", node.Name, rs2)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}

	// Both deployments should have restartedAt.
	for _, name := range []string{"app1", "app2"} {
		var dep appsv1.Deployment
		if err := te.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &dep); err != nil {
			t.Fatalf("get deployment %s: %v", name, err)
		}

		if _, ok := dep.Spec.Template.Annotations[AnnotationRestartedAt]; !ok {
			t.Errorf("expected restartedAt annotation on deployment %s", name)
		}
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-timeout-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})

	// Set processing-since to a time in the past (beyond timeout).
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[AnnotationProcessingSince] = time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	if err := te.client.Patch(ctx, node, patch); err != nil {
		t.Fatalf("patch node: %v", err)
	}

	r := newReconciler(te, func(r *NodeReconciler) {
		r.RolloutTimeout = 5 * time.Minute
	})
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after timeout, got %v", result.RequeueAfter)
	}

	// Verify processing-since annotation was removed and timed-out was set.
	var updatedNode corev1.Node
	if err := te.client.Get(ctx, types.NamespacedName{Name: node.Name}, &updatedNode); err != nil {
		t.Fatalf("get node: %v", err)
	}

	if _, ok := updatedNode.Annotations[AnnotationProcessingSince]; ok {
		t.Error("expected processing-since annotation to be removed after timeout")
	}

	if updatedNode.Annotations[AnnotationTimedOut] != "true" {
		t.Error("expected timed-out annotation to be set after timeout")
	}

	// A second reconcile should return immediately (no requeue, no re-processing).
	result2, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if result2.RequeueAfter != 0 {
		t.Errorf("expected no requeue on timed-out node, got %v", result2.RequeueAfter)
	}
}

func TestUnconfiguredTaint(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-unk-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "custom.io/some-taint", Effect: corev1.TaintEffectNoSchedule},
	})

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for unconfigured taint, got %v", result.RequeueAfter)
	}
}

func TestRequeueWhileRolloutInProgress(t *testing.T) {
	t.Parallel()

	te := setupTestEnv(t)
	ctx := context.Background()
	ns := fmt.Sprintf("test-requeue-%d", time.Now().UnixNano())
	createNamespace(t, ctx, te.client, ns)

	node := createNode(t, ctx, te.client, fmt.Sprintf("node-requeue-%d", time.Now().UnixNano()), []corev1.Taint{
		{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
	})

	// Set processing-since to recent time.
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[AnnotationProcessingSince] = time.Now().Format(time.RFC3339)
	if err := te.client.Patch(ctx, node, patch); err != nil {
		t.Fatalf("patch node: %v", err)
	}

	// Create a deployment with incomplete rollout (ReadyReplicas=0).
	deploy := createDeployment(t, ctx, te.client, ns, "myapp-rolling", 1, nil)
	rs := createReplicaSet(t, ctx, te.client, deploy)
	createPod(t, ctx, te.client, ns, "myapp-rolling-pod", node.Name, rs)

	r := newReconciler(te)
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s while rollout in progress, got %v", result.RequeueAfter)
	}
}
