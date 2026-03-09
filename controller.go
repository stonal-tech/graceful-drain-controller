package main

import (
	"context"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DeploymentReconciler watches Deployments that have the tracking annotation
// and handles cleanup after rollout completes or times out.
type DeploymentReconciler struct {
	client.Client
	Recorder       events.EventRecorder
	RolloutTimeout time.Duration
	RequeueInterval time.Duration
}

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	restartedAtStr, ok := deploy.Annotations[AnnotationDrainRestartedAt]
	if !ok {
		return reconcile.Result{}, nil
	}

	// Check if rollout is complete.
	if isRolloutComplete(&deploy) {
		slog.InfoContext(ctx, "rollout complete, removing tracking annotation",
			"deployment", deploy.Name, "namespace", deploy.Namespace)

		if err := r.removeTrackingAnnotation(ctx, &deploy); err != nil {
			return reconcile.Result{}, err
		}

		r.Recorder.Eventf(&deploy, nil, corev1.EventTypeNormal, "GracefulDrainCompleted", "RolloutComplete",
			"Graceful drain rollout completed successfully")

		return reconcile.Result{}, nil
	}

	// Check if timeout exceeded.
	restartedAt, err := time.Parse(time.RFC3339, restartedAtStr)
	if err != nil {
		slog.WarnContext(ctx, "invalid restarted-at annotation, removing",
			"deployment", deploy.Name, "namespace", deploy.Namespace, "value", restartedAtStr)

		if err := r.removeTrackingAnnotation(ctx, &deploy); err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if time.Since(restartedAt) > r.RolloutTimeout {
		slog.WarnContext(ctx, "rollout timeout reached, removing tracking annotation",
			"deployment", deploy.Name, "namespace", deploy.Namespace)

		if err := r.removeTrackingAnnotation(ctx, &deploy); err != nil {
			return reconcile.Result{}, err
		}

		r.Recorder.Eventf(&deploy, nil, corev1.EventTypeWarning, "GracefulDrainTimeout", "Timeout",
			"Graceful drain rollout timed out after %s", r.RolloutTimeout)

		return reconcile.Result{}, nil
	}

	// Rollout still in progress — requeue.
	slog.DebugContext(ctx, "rollout still in progress, requeueing",
		"deployment", deploy.Name, "namespace", deploy.Namespace)

	return reconcile.Result{RequeueAfter: r.RequeueInterval}, nil
}

func (r *DeploymentReconciler) removeTrackingAnnotation(ctx context.Context, deploy *appsv1.Deployment) error {
	patch := client.MergeFrom(deploy.DeepCopy())
	delete(deploy.Annotations, AnnotationDrainRestartedAt)

	return r.Patch(ctx, deploy, patch)
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return hasTrackingAnnotation(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return hasTrackingAnnotation(e.ObjectNew)
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

func hasTrackingAnnotation(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, ok := annotations[AnnotationDrainRestartedAt]
	return ok
}
