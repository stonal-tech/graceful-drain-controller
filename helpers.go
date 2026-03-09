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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AnnotationDrainRestartedAt is set on the Deployment metadata (not pod template)
	// to track webhook-initiated rollouts. Value is RFC3339 timestamp.
	AnnotationDrainRestartedAt = "graceful-drain.stonal.com/restarted-at"

	// AnnotationRestartedAt is the standard kubectl restart annotation on pod template.
	AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
)

// getOwningDeployment walks pod -> ReplicaSet -> Deployment via OwnerReferences.
func getOwningDeployment(ctx context.Context, c client.Client, pod *corev1.Pod) (*appsv1.Deployment, error) {
	rsRef := getOwnerRef(pod.OwnerReferences, "ReplicaSet")
	if rsRef == nil {
		return nil, nil //nolint:nilnil // nil,nil means no owner found, not an error
	}

	var rs appsv1.ReplicaSet
	if err := c.Get(ctx, types.NamespacedName{Name: rsRef.Name, Namespace: pod.Namespace}, &rs); err != nil {
		return nil, fmt.Errorf("get replicaset %s/%s: %w", pod.Namespace, rsRef.Name, err)
	}

	deployRef := getOwnerRef(rs.OwnerReferences, "Deployment")
	if deployRef == nil {
		return nil, nil //nolint:nilnil // nil,nil means no owner found, not an error
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
// It verifies the deployment controller has observed the latest spec change
// before checking replica counts, to avoid false positives from stale status.
func isRolloutComplete(deploy *appsv1.Deployment) bool {
	if deploy.Spec.Replicas == nil {
		return false
	}

	// Ensure the deployment controller has processed the latest generation.
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}

	return deploy.Status.UpdatedReplicas == *deploy.Spec.Replicas &&
		deploy.Status.ReadyReplicas == *deploy.Spec.Replicas &&
		deploy.Status.UnavailableReplicas == 0
}

// triggerRolloutRestart patches the restartedAt annotation on the deployment's pod template
// and sets the tracking annotation on the deployment metadata.
func triggerRolloutRestart(ctx context.Context, c client.Client, deploy *appsv1.Deployment) error {
	patch := client.MergeFrom(deploy.DeepCopy())
	now := time.Now().Format(time.RFC3339)

	// Set tracking annotation on deployment metadata.
	if deploy.Annotations == nil {
		deploy.Annotations = make(map[string]string)
	}

	deploy.Annotations[AnnotationDrainRestartedAt] = now

	// Set restart annotation on pod template.
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}

	deploy.Spec.Template.Annotations[AnnotationRestartedAt] = now

	return c.Patch(ctx, deploy, patch)
}

// warnIfBadStrategy logs a warning if the deployment doesn't have the recommended rolling update strategy.
func warnIfBadStrategy(ctx context.Context, deploy *appsv1.Deployment) {
	if deploy.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		slog.WarnContext(ctx, "deployment does not use RollingUpdate strategy, rollout restart may cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace, "strategy", deploy.Spec.Strategy.Type)

		return
	}

	ru := deploy.Spec.Strategy.RollingUpdate
	if ru == nil {
		return
	}

	if ru.MaxSurge != nil && ru.MaxSurge.IntValue() == 0 {
		slog.WarnContext(ctx, "deployment has maxSurge=0, rollout restart will cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
	}

	if ru.MaxUnavailable != nil && ru.MaxUnavailable.IntValue() > 0 {
		slog.WarnContext(ctx, "deployment has maxUnavailable>0, rollout restart may cause downtime",
			"deployment", deploy.Name, "namespace", deploy.Namespace)
	}
}
