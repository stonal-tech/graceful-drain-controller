package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// Annotation set on Nodes being processed (value = RFC3339 timestamp of when processing started).
	AnnotationProcessingSince = "graceful-drain.stonal.com/processing-since"

	// Standard kubectl restart annotation.
	AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
)

// DrainTaint represents a taint key+effect pair to watch for on nodes.
type DrainTaint struct {
	Key    string
	Effect string
}

// NodeReconciler reconciles Node objects that have drain taints.
type NodeReconciler struct {
	client.Client
	Recorder          record.EventRecorder
	DrainTaints       []DrainTaint
	EnabledAnnotation string
	RequeueInterval   time.Duration
	RolloutTimeout    time.Duration
}

func (r *NodeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !hasDrainTaint(&node, r.DrainTaints) {
		return reconcile.Result{}, nil
	}

	// Check if we're already processing this node.
	if sinceStr, ok := node.Annotations[AnnotationProcessingSince]; ok {
		sinceTime, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			slog.Warn("invalid processing-since annotation, ignoring", "node", node.Name, "value", sinceStr)
			return reconcile.Result{}, nil
		}

		if time.Since(sinceTime) > r.RolloutTimeout {
			slog.Warn("rollout timeout reached, letting autoscaler force-drain", "node", node.Name, "timeout", r.RolloutTimeout)
			r.emitTimeoutEvents(ctx, &node)
			return reconcile.Result{}, nil
		}

		// Re-scan: check if any eligible deployments still have incomplete rollouts.
		done, err := r.allRolloutsComplete(ctx, &node)
		if err != nil {
			return reconcile.Result{}, err
		}
		if done {
			slog.Info("all rollouts complete, node can drain", "node", node.Name)
			return reconcile.Result{}, nil
		}

		slog.Debug("rollouts still in progress, requeueing", "node", node.Name)
		return reconcile.Result{RequeueAfter: r.RequeueInterval}, nil
	}

	// First time seeing this drained node — find and restart eligible deployments.
	pods, err := r.listRunningPods(ctx, node.Name)
	if err != nil {
		return reconcile.Result{}, err
	}

	var targetedDeployments []*appsv1.Deployment
	for i := range pods {
		pod := &pods[i]
		deploy, err := getOwningDeployment(ctx, r.Client, pod)
		if err != nil {
			slog.Warn("failed to resolve owning deployment", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
			continue
		}
		if deploy == nil {
			continue
		}
		if deploy.DeletionTimestamp != nil {
			continue
		}
		if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
			continue
		}
		if r.EnabledAnnotation != "" {
			if deploy.Annotations == nil || deploy.Annotations[r.EnabledAnnotation] != "true" {
				continue
			}
		}
		if isAlreadyRestarting(deploy, r.RolloutTimeout) {
			slog.Debug("deployment already restarting, skipping", "deployment", deploy.Name, "namespace", deploy.Namespace)
			continue
		}

		r.warnIfBadStrategy(deploy)

		if err := triggerRolloutRestart(ctx, r.Client, deploy); err != nil {
			slog.Error("failed to trigger rollout restart", "deployment", deploy.Name, "namespace", deploy.Namespace, "error", err)
			continue
		}

		slog.Info("triggered rollout restart", "deployment", deploy.Name, "namespace", deploy.Namespace, "node", node.Name)
		r.Recorder.Eventf(deploy, corev1.EventTypeNormal, "GracefulDrainTriggered",
			"Triggered rollout restart due to node %s being drained", node.Name)
		targetedDeployments = append(targetedDeployments, deploy)
	}

	if len(targetedDeployments) > 0 {
		if err := r.annotateNodeProcessing(ctx, &node); err != nil {
			return reconcile.Result{}, err
		}
		slog.Info("marked node as processing", "node", node.Name, "deployments", len(targetedDeployments))
		return reconcile.Result{RequeueAfter: r.RequeueInterval}, nil
	}

	return reconcile.Result{}, nil
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	drainTaints := r.DrainTaints
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				node, ok := e.Object.(*corev1.Node)
				if !ok {
					return false
				}
				return hasDrainTaint(node, drainTaints)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				node, ok := e.ObjectNew.(*corev1.Node)
				if !ok {
					return false
				}
				return hasDrainTaint(node, drainTaints)
			},
			DeleteFunc: func(_ event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(_ event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}

// hasDrainTaint checks if the node has any of the configured drain taints.
func hasDrainTaint(node *corev1.Node, drainTaints []DrainTaint) bool {
	for _, taint := range node.Spec.Taints {
		for _, dt := range drainTaints {
			if taint.Key == dt.Key && string(taint.Effect) == dt.Effect {
				return true
			}
		}
	}
	return false
}

// getOwningDeployment walks pod → ReplicaSet → Deployment via OwnerReferences.
func getOwningDeployment(ctx context.Context, c client.Client, pod *corev1.Pod) (*appsv1.Deployment, error) {
	rsRef := getOwnerRef(pod.OwnerReferences, "ReplicaSet")
	if rsRef == nil {
		return nil, nil
	}

	var rs appsv1.ReplicaSet
	if err := c.Get(ctx, types.NamespacedName{Name: rsRef.Name, Namespace: pod.Namespace}, &rs); err != nil {
		return nil, fmt.Errorf("get replicaset %s/%s: %w", pod.Namespace, rsRef.Name, err)
	}

	deployRef := getOwnerRef(rs.OwnerReferences, "Deployment")
	if deployRef == nil {
		return nil, nil
	}

	var deploy appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: deployRef.Name, Namespace: rs.Namespace}, &deploy); err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", rs.Namespace, deployRef.Name, err)
	}

	return &deploy, nil
}

func getOwnerRef(refs []metav1.OwnerReference, kind string) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Kind == kind {
			return &refs[i]
		}
	}
	return nil
}

// isRolloutComplete checks if a deployment's rollout is fully done.
func isRolloutComplete(deploy *appsv1.Deployment) bool {
	return deploy.Status.UpdatedReplicas == *deploy.Spec.Replicas &&
		deploy.Status.ReadyReplicas == *deploy.Spec.Replicas &&
		deploy.Status.UnavailableReplicas == 0
}

// triggerRolloutRestart patches the restartedAt annotation on the deployment's pod template.
func triggerRolloutRestart(ctx context.Context, c client.Client, deploy *appsv1.Deployment) error {
	patch := client.MergeFrom(deploy.DeepCopy())
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations[AnnotationRestartedAt] = time.Now().Format(time.RFC3339)
	return c.Patch(ctx, deploy, patch)
}

// isAlreadyRestarting checks if the deployment was recently restarted.
func isAlreadyRestarting(deploy *appsv1.Deployment, within time.Duration) bool {
	if deploy.Spec.Template.Annotations == nil {
		return false
	}
	restartedAt, ok := deploy.Spec.Template.Annotations[AnnotationRestartedAt]
	if !ok {
		return false
	}
	t, err := time.Parse(time.RFC3339, restartedAt)
	if err != nil {
		return false
	}
	return time.Since(t) < within
}

// listRunningPods lists non-terminating pods on the given node.
func (r *NodeReconciler) listRunningPods(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", nodeName, err)
	}

	var running []corev1.Pod
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			running = append(running, pod)
		}
	}
	return running, nil
}

// allRolloutsComplete checks if all eligible deployments on the node have completed their rollouts.
func (r *NodeReconciler) allRolloutsComplete(ctx context.Context, node *corev1.Node) (bool, error) {
	pods, err := r.listRunningPods(ctx, node.Name)
	if err != nil {
		return false, err
	}

	if len(pods) == 0 {
		return true, nil
	}

	for i := range pods {
		deploy, err := getOwningDeployment(ctx, r.Client, &pods[i])
		if err != nil {
			slog.Warn("failed to resolve owning deployment during rollout check", "pod", pods[i].Name, "error", err)
			continue
		}
		if deploy == nil || deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
			continue
		}
		if r.EnabledAnnotation != "" {
			if deploy.Annotations == nil || deploy.Annotations[r.EnabledAnnotation] != "true" {
				continue
			}
		}
		if !isRolloutComplete(deploy) {
			return false, nil
		}
	}

	return true, nil
}

// annotateNodeProcessing sets the processing-since annotation on the node.
func (r *NodeReconciler) annotateNodeProcessing(ctx context.Context, node *corev1.Node) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[AnnotationProcessingSince] = time.Now().Format(time.RFC3339)
	return r.Patch(ctx, node, patch)
}

// emitTimeoutEvents emits timeout warning events on eligible deployments still on the node.
func (r *NodeReconciler) emitTimeoutEvents(ctx context.Context, node *corev1.Node) {
	pods, err := r.listRunningPods(ctx, node.Name)
	if err != nil {
		slog.Error("failed to list pods for timeout events", "node", node.Name, "error", err)
		return
	}
	for i := range pods {
		deploy, err := getOwningDeployment(ctx, r.Client, &pods[i])
		if err != nil || deploy == nil {
			continue
		}
		if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
			continue
		}
		r.Recorder.Eventf(deploy, corev1.EventTypeWarning, "GracefulDrainTimeout",
			"Rollout timeout reached for node %s, letting autoscaler force-drain", node.Name)
	}
}

// warnIfBadStrategy logs a warning if the deployment doesn't have the recommended rolling update strategy.
func (r *NodeReconciler) warnIfBadStrategy(deploy *appsv1.Deployment) {
	if deploy.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		slog.Warn("deployment does not use RollingUpdate strategy, rollout restart may cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace, "strategy", deploy.Spec.Strategy.Type)
		return
	}
	ru := deploy.Spec.Strategy.RollingUpdate
	if ru == nil {
		return
	}
	if ru.MaxSurge != nil && ru.MaxSurge.IntValue() == 0 {
		slog.Warn("deployment has maxSurge=0, rollout restart will cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
	}
	if ru.MaxUnavailable != nil && ru.MaxUnavailable.IntValue() > 0 {
		slog.Warn("deployment has maxUnavailable>0, rollout restart may cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
	}
}
