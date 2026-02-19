# Karpenter Graceful Drain Controller

## Problem

We use Karpenter for node autoscaling on our Kubernetes clusters. Many of our Deployments run with `replicas: 1`. When Karpenter decides to consolidate or disrupt a node, it taints the node with `karpenter.sh/disrupted:NoSchedule` and **immediately** begins evicting pods via the Kubernetes Eviction API. There is no configurable delay between the taint and the eviction.

For replica=1 Deployments, this causes a dilemma:
- **No PDB**: Karpenter evicts the pod instantly. Downtime until the replacement starts elsewhere.
- **PDB with `minAvailable: 1`**: Karpenter's eviction is blocked by the PDB. The node can never be drained. Karpenter logs `Unconsolidatable: pdb prevents pod evictions` and gives up (or force-drains after `terminationGracePeriod`).

Neither option gives us zero-downtime consolidation.

This is a well-known issue:
- https://github.com/kubernetes-sigs/karpenter/issues/2600
- https://github.com/aws/karpenter-provider-aws/issues/500

### Karpenter disruption sequence (for context)

This is the exact sequence Karpenter follows when disrupting a node:

1. Identify candidate nodes for disruption (consolidation, drift, expiration).
2. Run a scheduling simulation to check if pods can be placed elsewhere.
3. **Taint** the node with `karpenter.sh/disrupted:NoSchedule`.
4. **Immediately begin evicting** pods via the Kubernetes Eviction API (respects PDBs).
5. Wait for the node to be fully drained.
6. Terminate the NodeClaim in the cloud provider.
7. Remove the node from the API server.

Steps 3 and 4 are back-to-back — there is no pause between the taint and eviction. The only thing that delays eviction is a PDB or the pod's `terminationGracePeriodSeconds`. If a PDB blocks eviction, Karpenter retries until the NodePool's `terminationGracePeriod` is reached, at which point it force-deletes pods.

## Solution

Build a custom Kubernetes controller that **breaks the PDB deadlock** for replica=1 Deployments during Karpenter node disruption.

The approach relies on the combination of:
1. **A PDB with `minAvailable: 1`** on each singleton Deployment — this blocks Karpenter's immediate eviction, buying time.
2. **The controller** watches for the Karpenter disruption taint, then triggers a rollout restart on affected Deployments.
3. The rollout restart creates a **surge pod** on a healthy node (the disrupted node is tainted `NoSchedule`, so the new pod lands elsewhere).
4. Once the new pod is Ready, there are temporarily 2 pods running. The PDB (`minAvailable: 1`) now allows evicting 1 pod.
5. Karpenter's eviction retry succeeds, the old pod is evicted, the node drains, and Karpenter terminates it.

This is a **cooperative dance** between the PDB, the controller, and Karpenter's eviction retry loop.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Karpenter decides to disrupt Node X                             │
│    │                                                             │
│    ├─► Taints Node X: karpenter.sh/disrupted:NoSchedule          │
│    ├─► Tries to evict Pod A (replica=1, PDB minAvailable=1)      │
│    └─► BLOCKED by PDB — retries in a loop                        │
│                                                                  │
│  Our controller (running in parallel):                           │
│    │                                                             │
│    ├─► Sees taint on Node X                                      │
│    ├─► Finds Pod A → Deployment D (replicas=1, opted-in)         │
│    ├─► Triggers rollout restart on Deployment D                  │
│    │     └─► K8s creates Pod B on a healthy node                 │
│    │         (maxSurge=1, maxUnavailable=0)                      │
│    ├─► Pod B becomes Ready                                       │
│    │     └─► Deployment D now has 2 running pods temporarily     │
│    │                                                             │
│    └─► PDB satisfied (2 pods, minAvailable=1 → 1 eviction OK)   │
│          └─► Karpenter's eviction retry succeeds                 │
│              └─► Pod A evicted, Node X drains, terminated        │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

## Detailed Behavior

### Opt-in mechanism

Only Deployments annotated with `graceful-drain.stonal.com/enabled: "true"` are handled. This avoids interfering with workloads that don't need this behavior.

### Trigger

The controller watches `Node` objects. It reconciles when a Node gains the taint `karpenter.sh/disrupted` with effect `NoSchedule`. This taint is added by Karpenter when it decides to disrupt/consolidate a node.

### Reconciliation logic

```
func Reconcile(node):
    if node does not have karpenter disruption taint:
        return (no requeue)

    if node is already being processed (check annotation on node):
        # Check progress of in-flight rollouts
        if all targeted deployments have completed rollout:
            annotate node as "drain-ready" (for observability)
            return (no requeue)
        else if processing started more than RolloutTimeout ago:
            log warning "rollout timeout, letting Karpenter force-drain"
            return (no requeue)
        else:
            return (requeue after 5s)

    pods = list pods on this node (running, not terminating)

    targetedDeployments = []

    for each pod in pods:
        deployment = resolve owning Deployment (pod -> ReplicaSet -> Deployment)

        if deployment is nil:
            continue
        if deployment.spec.replicas != 1:
            continue
        if deployment does not have annotation "graceful-drain.stonal.com/enabled: true":
            continue
        if deployment is already restarting (check restartedAt annotation is recent):
            continue

        # Trigger rollout restart by patching the pod template annotation
        patch deployment.spec.template.metadata.annotations:
            "kubectl.kubernetes.io/restartedAt" = now().Format(RFC3339)

        emit Event on Deployment: Normal / GracefulDrainTriggered
        append deployment to targetedDeployments

    if len(targetedDeployments) > 0:
        annotate node with:
            "graceful-drain.stonal.com/processing": "true"
            "graceful-drain.stonal.com/processing-since": now().Format(RFC3339)
        return (requeue after 5s)

    return (no requeue)
```

### Deployment prerequisites

For this to work, each target Deployment MUST have all three of these configured:

**1. Rolling update strategy with surge:**

```yaml
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1        # Allow creating a new pod before killing the old one
      maxUnavailable: 0   # Never kill the old pod until the new one is Ready
```

This is what allows Kubernetes to temporarily run 2 pods during the rollout. Without `maxSurge: 1`, the rollout restart would first kill the old pod (causing downtime). Without `maxUnavailable: 0`, same problem.

**2. A PodDisruptionBudget:**

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: my-singleton-app
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: my-singleton-app
```

This is **mandatory**. The PDB is what blocks Karpenter's immediate eviction and gives the controller time to trigger the rollout. Without a PDB, Karpenter evicts the pod instantly and there is nothing the controller can do.

**3. The opt-in annotation:**

```yaml
metadata:
  annotations:
    graceful-drain.stonal.com/enabled: "true"
```

### Why PDB is mandatory (the race condition)

When Karpenter disrupts a node, it taints it and **immediately** starts evicting pods. There is no delay. If there is no PDB:

- Karpenter sends eviction request → Kubernetes accepts it → pod starts terminating
- Our controller sees the taint → triggers rollout restart → but the old pod is already dying
- The rollout creates a new pod, but there is a gap where 0 pods are running

The PDB prevents this race by making the eviction request fail with `429 Too Many Requests` (disruption budget violation). Karpenter retries eviction in a loop. Meanwhile, our controller triggers the rollout, the surge pod comes up, and on the next retry Karpenter's eviction succeeds because the PDB is now satisfied (2 pods running, 1 eviction allowed).

### Timing considerations

Typical timeline:
- T+0s: Karpenter taints node, immediately tries eviction → PDB blocks it
- T+0s to T+5s: Our controller reconciles, sees the taint, triggers rollout restart
- T+5s to T+60s: New pod is being scheduled, image pulled, started, passes readiness probe
- T+60s: New pod is Ready, Deployment has 2 running pods
- T+60s+: Karpenter retries eviction → PDB allows it (2 pods, minAvailable=1) → old pod evicted
- T+60s++: Node fully drained, Karpenter terminates the node

The NodePool's `terminationGracePeriod` (default 24h, often set to 1h) must be long enough for the rollout to complete. If the rollout takes too long (image pull issues, crashloop), Karpenter will eventually force-drain after this timeout.

## Project Structure

Use kubebuilder to scaffold the project. The project should be a standalone Go module.

```
karpenter-graceful-drain/
├── cmd/
│   └── main.go                      # Entry point, sets up manager
├── internal/
│   └── controller/
│       ├── node_controller.go       # Main reconciler watching Nodes
│       └── node_controller_test.go  # Unit tests with envtest
├── Dockerfile                       # Multi-stage build
├── deploy/
│   ├── helm/
│   │   └── karpenter-graceful-drain/
│   │       ├── Chart.yaml
│   │       ├── values.yaml
│   │       └── templates/
│   │           ├── deployment.yaml
│   │           ├── serviceaccount.yaml
│   │           ├── clusterrole.yaml
│   │           └── clusterrolebinding.yaml
│   └── kustomize/                   # Alternative to Helm
│       ├── kustomization.yaml
│       └── manager.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## Controller Implementation Details

### main.go

- Create a controller-runtime `Manager`
- Register the Node reconciler
- Add health/ready probes on `:8081`
- Leader election enabled (controller should run with replicas=1 or with leader election)

### node_controller.go

**Struct:**

```go
type NodeReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}
```

**Watches:**
- Primary: `Node` objects
- The reconciler should use a predicate to only enqueue Nodes that have the `karpenter.sh/disrupted` taint (use an `EventFilter` on Create/Update).

**Constants / Config:**

```go
const (
    // Annotation on Deployments to opt in
    AnnotationEnabled = "graceful-drain.stonal.com/enabled"

    // Annotation set on Nodes being processed
    AnnotationProcessing = "graceful-drain.stonal.com/processing"

    // Annotation to track when processing started
    AnnotationProcessingSince = "graceful-drain.stonal.com/processing-since"

    // Standard kubectl restart annotation
    AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"

    // Karpenter disruption taint key
    KarpenterDisruptionTaintKey = "karpenter.sh/disrupted"

    // Requeue interval while waiting for rollouts
    RequeueInterval = 5 * time.Second

    // Max time to wait for a rollout before giving up
    // After this, Karpenter's own terminationGracePeriod takes over
    RolloutTimeout = 5 * time.Minute
)
```

**Key helper functions:**

1. `hasKarpenterDisruptionTaint(node *corev1.Node) bool` — checks for the taint `karpenter.sh/disrupted` with effect `NoSchedule`
2. `getOwningDeployment(ctx, client, pod) (*appsv1.Deployment, error)` — walks pod → ReplicaSet → Deployment via OwnerReferences
3. `isRolloutComplete(deployment *appsv1.Deployment) bool` — checks `deployment.Status.UpdatedReplicas == deployment.Status.Replicas && deployment.Status.ReadyReplicas == deployment.Status.Replicas && deployment.Status.UnavailableReplicas == 0`
4. `triggerRolloutRestart(ctx, client, deployment) error` — patches the `restartedAt` annotation on the pod template
5. `isAlreadyRestarting(deployment *appsv1.Deployment, since time.Duration) bool` — checks if `restartedAt` annotation is recent (within `since` duration)

**Events:**
The controller should emit Kubernetes Events on the Deployment:
- `Normal` / `GracefulDrainTriggered` — when a rollout restart is triggered
- `Warning` / `GracefulDrainTimeout` — if rollout didn't complete within timeout
- `Normal` / `GracefulDrainCompleted` — when rollout is confirmed complete

**Metrics (optional but nice):**
- `graceful_drain_rollouts_triggered_total` (counter)
- `graceful_drain_rollouts_completed_total` (counter)
- `graceful_drain_rollout_duration_seconds` (histogram)

### RBAC Requirements

The controller needs these ClusterRole permissions:

```yaml
rules:
  # Watch and annotate nodes
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch"]
  # List pods
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  # Get ReplicaSets (to resolve ownership)
  - apiGroups: ["apps"]
    resources: ["replicasets"]
    verbs: ["get", "list"]
  # Patch Deployments (trigger rollout restart)
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "patch"]
  # Emit events
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
```

## Testing Strategy

### Unit tests (envtest)

Use controller-runtime's `envtest` to:
1. Create a Node with the Karpenter taint
2. Create a Deployment with replicas=1 and the opt-in annotation
3. Create a Pod on that Node owned by the Deployment (via a ReplicaSet)
4. Run the reconciler
5. Assert that the Deployment's pod template now has the `restartedAt` annotation
6. Assert that the Node has the `processing` annotation

Test cases:
- Happy path: single Deployment, single pod, triggers restart
- Skip: Deployment without opt-in annotation → no restart
- Skip: Deployment with replicas > 1 → no restart
- Skip: Node without Karpenter taint → no action
- Idempotency: already-restarting Deployment → no re-trigger
- Multiple Deployments on same node → all eligible ones get restarted
- Timeout: processing-since is older than RolloutTimeout → stop requeueing

### Integration / E2E (optional)

A simple test in a Kind cluster with a fake "disruption" by manually tainting a node.

## Dockerfile

Multi-stage build:

```dockerfile
FROM golang:1.23 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o manager cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
```

## Helm Values

```yaml
replicaCount: 1

image:
  repository: your-registry/karpenter-graceful-drain
  tag: latest
  pullPolicy: IfNotPresent

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    memory: 128Mi

leaderElection:
  enabled: true

# Log level: info, debug
logLevel: info
```

## How to Use

### 1. Deploy the controller

```bash
helm install karpenter-graceful-drain ./deploy/helm/karpenter-graceful-drain -n kube-system
```

### 2. Configure target Deployments

Each singleton Deployment needs three things:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-singleton-app
  annotations:
    graceful-drain.stonal.com/enabled: "true"   # Opt-in to the controller
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1           # REQUIRED: allow surge pod during rollout
      maxUnavailable: 0     # REQUIRED: don't kill old pod until new is Ready
  selector:
    matchLabels:
      app: my-singleton-app
  template:
    metadata:
      labels:
        app: my-singleton-app
    spec:
      containers:
        - name: app
          image: my-app:latest
          readinessProbe:    # IMPORTANT: must have a readiness probe
            httpGet:         # so K8s knows when the new pod is truly ready
              path: /healthz
              port: 8080
```

### 3. Create a PDB for each target Deployment (MANDATORY)

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: my-singleton-app
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: my-singleton-app
```

**This PDB is mandatory.** Without it, Karpenter evicts the pod instantly before the controller can act. The PDB blocks eviction and buys time for the rollout to complete.

### 4. Ensure readiness probes are configured

The rollout restart relies on the new pod passing its readiness probe before the old pod is terminated. Without a readiness probe, Kubernetes considers the pod Ready as soon as it starts, which may be too early (the application might not be accepting traffic yet).

## Edge Cases to Handle

1. **Pod with no Deployment owner** (e.g., StatefulSet, bare pod): Skip. This controller only handles Deployments.
2. **Deployment already being deleted**: Skip (check DeletionTimestamp).
3. **Node removed before rollout completes**: The rollout continues normally since the new pod is on a different node. The old pod will be garbage collected. No harm done.
4. **Controller restarts during processing**: The node still has the taint, so the controller will re-reconcile. The `isAlreadyRestarting` check prevents re-triggering. The `processing` and `processing-since` annotations help track state.
5. **Rollout stuck (image pull error, crashloop, etc.)**: The timeout mechanism (RolloutTimeout = 5min) stops the controller from requeueing forever. After timeout, Karpenter's own `terminationGracePeriod` will eventually force-drain the node.
6. **Karpenter eviction retry timing**: Karpenter retries evictions continuously while waiting for the node to drain. As soon as the surge pod is Ready and the PDB is satisfied, the next retry will succeed. There is no manual step needed to "unblock" Karpenter.
7. **Multiple nodes disrupted simultaneously**: Each node is reconciled independently. The controller processes each node's pods separately. If two nodes have pods from the same Deployment, the `isAlreadyRestarting` check prevents double-triggering.
8. **Deployment with `maxSurge: 0` or `maxUnavailable: 1`**: The rollout restart will kill the old pod before creating a new one, causing downtime despite the PDB. The controller should log a warning if it detects this misconfiguration.

## Go Dependencies

```
k8s.io/api
k8s.io/apimachinery
k8s.io/client-go
sigs.k8s.io/controller-runtime
```

Target Go version: 1.23+
Target controller-runtime: v0.19+ (compatible with Kubernetes 1.31+)
