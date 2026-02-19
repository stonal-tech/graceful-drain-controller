# Graceful Drain Controller

## Problem

When a node autoscaler (Karpenter, Cluster Autoscaler, or any custom drain process) decides to remove a node, it typically taints the node and then evicts pods via the Kubernetes Eviction API. Many of our Deployments run with `replicas: 1`. This causes a dilemma:

- **No PDB**: The autoscaler evicts the pod instantly. Downtime until the replacement starts elsewhere.
- **PDB with `minAvailable: 1`**: The eviction is blocked by the PDB. The node can never be drained. The autoscaler gives up or force-drains after a timeout.

Neither option gives us zero-downtime node drains for singleton Deployments.

### Common autoscaler drain sequences

All major autoscalers follow the same general pattern:

1. Identify candidate nodes for removal (consolidation, scale-down, drift, expiration).
2. **Taint** the node to prevent new pods from scheduling on it.
3. **Evict** pods via the Kubernetes Eviction API (respects PDBs).
4. Wait for the node to be fully drained.
5. Terminate/remove the node.

The specific taint varies by autoscaler:

| Autoscaler | Taint Key | Effect |
|---|---|---|
| **Karpenter** | `karpenter.sh/disrupted` | `NoSchedule` |
| **Cluster Autoscaler** | `ToBeDeletedByClusterAutoscaler` | `NoSchedule` |
| **kubectl drain / cordon** | `node.kubernetes.io/unschedulable` | `NoSchedule` |

The eviction typically starts immediately after the taint is applied. The only thing that delays eviction is a PDB or the pod's `terminationGracePeriodSeconds`.

This is a well-known issue in the Karpenter ecosystem specifically:
- https://github.com/kubernetes-sigs/karpenter/issues/2600
- https://github.com/aws/karpenter-provider-aws/issues/500

But the problem is fundamentally the same for any autoscaler that taints-then-evicts.

## Solution

Build a custom Kubernetes controller that **breaks the PDB deadlock** for replica=1 Deployments during node drain, regardless of which autoscaler or drain mechanism is used.

The approach relies on the combination of:
1. **A PDB with `minAvailable: 1`** on each singleton Deployment — this blocks the autoscaler's immediate eviction, buying time.
2. **The controller** watches for configurable drain taints on nodes, then triggers a rollout restart on affected Deployments.
3. The rollout restart creates a **surge pod** on a healthy node (the draining node is tainted `NoSchedule`, so the new pod lands elsewhere).
4. Once the new pod is Ready, there are temporarily 2 pods running. The PDB (`minAvailable: 1`) now allows evicting 1 pod.
5. The autoscaler's eviction retry succeeds, the old pod is evicted, the node drains, and the autoscaler terminates it.

This is a **cooperative dance** between the PDB, the controller, and the autoscaler's eviction retry loop.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Autoscaler decides to drain Node X                              │
│    │                                                             │
│    ├─► Taints Node X (e.g. karpenter.sh/disrupted:NoSchedule)   │
│    ├─► Tries to evict Pod A (replica=1, PDB minAvailable=1)     │
│    └─► BLOCKED by PDB — retries in a loop                       │
│                                                                  │
│  Our controller (running in parallel):                           │
│    │                                                             │
│    ├─► Sees drain taint on Node X                                │
│    ├─► Finds Pod A → Deployment D (replicas=1, opted-in)        │
│    ├─► Triggers rollout restart on Deployment D                  │
│    │     └─► K8s creates Pod B on a healthy node                │
│    │         (maxSurge=1, maxUnavailable=0)                     │
│    ├─► Pod B becomes Ready                                       │
│    │     └─► Deployment D now has 2 running pods temporarily    │
│    │                                                             │
│    └─► PDB satisfied (2 pods, minAvailable=1 → 1 eviction OK)  │
│          └─► Autoscaler's eviction retry succeeds                │
│              └─► Pod A evicted, Node X drains, terminated       │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

## Detailed Behavior

### Opt-in mechanism

Only Deployments annotated with `graceful-drain.stonal.com/enabled: "true"` are handled. This avoids interfering with workloads that don't need this behavior.

### Configurable drain taints

The controller watches for configurable taints on nodes. By default, it watches for:
- `karpenter.sh/disrupted` (Karpenter)
- `ToBeDeletedByClusterAutoscaler` (Cluster Autoscaler)
- `node.kubernetes.io/unschedulable` (kubectl drain/cordon)

Additional taints can be configured via Helm values or command-line flags.

### Trigger

The controller watches `Node` objects. It reconciles when a Node gains any of the configured drain taints. This allows it to work with Karpenter, Cluster Autoscaler, manual drains, or any custom node lifecycle controller.

### Reconciliation logic

```
func Reconcile(node):
    if node does not have any configured drain taint:
        return (no requeue)

    if node is already being processed (check annotation on node):
        # Check progress of in-flight rollouts
        if all targeted deployments have completed rollout:
            annotate node as "drain-ready" (for observability)
            return (no requeue)
        else if processing started more than RolloutTimeout ago:
            log warning "rollout timeout, letting autoscaler force-drain"
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

This is **mandatory**. The PDB is what blocks the autoscaler's immediate eviction and gives the controller time to trigger the rollout. Without a PDB, the autoscaler evicts the pod instantly and there is nothing the controller can do.

**3. The opt-in annotation:**

```yaml
metadata:
  annotations:
    graceful-drain.stonal.com/enabled: "true"
```

### Why PDB is mandatory (the race condition)

When an autoscaler drains a node, it taints it and **immediately** starts evicting pods. There is no delay. If there is no PDB:

- Autoscaler sends eviction request → Kubernetes accepts it → pod starts terminating
- Our controller sees the taint → triggers rollout restart → but the old pod is already dying
- The rollout creates a new pod, but there is a gap where 0 pods are running

The PDB prevents this race by making the eviction request fail with `429 Too Many Requests` (disruption budget violation). The autoscaler retries eviction in a loop. Meanwhile, our controller triggers the rollout, the surge pod comes up, and on the next retry the eviction succeeds because the PDB is now satisfied (2 pods running, 1 eviction allowed).

### Timing considerations

Typical timeline:
- T+0s: Autoscaler taints node, immediately tries eviction → PDB blocks it
- T+0s to T+5s: Our controller reconciles, sees the taint, triggers rollout restart
- T+5s to T+60s: New pod is being scheduled, image pulled, started, passes readiness probe
- T+60s: New pod is Ready, Deployment has 2 running pods
- T+60s+: Autoscaler retries eviction → PDB allows it (2 pods, minAvailable=1) → old pod evicted
- T+60s++: Node fully drained, autoscaler terminates the node

The autoscaler's drain timeout (e.g. Karpenter's `terminationGracePeriod`, Cluster Autoscaler's `max-graceful-termination-sec`) must be long enough for the rollout to complete. If the rollout takes too long (image pull issues, crashloop), the autoscaler will eventually force-drain after this timeout.

## Project Structure

Use kubebuilder to scaffold the project. The project should be a standalone Go module.

```
graceful-drain-controller/
├── cmd/
│   └── main.go                      # Entry point: urfave/cli app, koanf config loading, manager setup
├── internal/
│   ├── config/
│   │   └── config.go               # Config struct, defaults, koanf loading
│   └── controller/
│       ├── node_controller.go       # Main reconciler watching Nodes
│       └── node_controller_test.go  # Unit tests with envtest
├── Dockerfile                       # Multi-stage build
├── deploy/
│   ├── helm/
│   │   └── graceful-drain-controller/
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

- Use `urfave/cli/v3` to define the CLI app and flags
- Use `koanf/v2` to load configuration from (in order of priority):
  1. Default values (struct tags or hardcoded)
  2. Config file (optional, YAML/JSON via `--config` flag)
  3. Environment variables (prefix `GRACEFUL_DRAIN_`)
  4. CLI flags (highest priority)
- Create a controller-runtime `Manager`
- Register the Node reconciler
- Add health/ready probes on `:8081`
- Leader election enabled (controller should run with replicas=1 or with leader election)

**CLI flags (via urfave/cli v3):**

```
--config             Path to config file (YAML)
--leader-elect       Enable leader election (default: true)
--health-probe-addr  Address for health probes (default: ":8081")
--metrics-addr       Address for metrics endpoint (default: ":8080")
--log-level          Log level: debug, info, warn, error (default: "info")
--drain-taint        Drain taints to watch, repeatable (default: karpenter.sh/disrupted:NoSchedule, ToBeDeletedByClusterAutoscaler:NoSchedule, node.kubernetes.io/unschedulable:NoSchedule)
--requeue-interval   Requeue interval while waiting for rollouts (default: 5s)
--rollout-timeout    Max time to wait for a rollout (default: 5m)
```

**Config struct (loaded by koanf):**

```go
type Config struct {
    LeaderElect     bool          `koanf:"leader_elect"`
    HealthProbeAddr string        `koanf:"health_probe_addr"`
    MetricsAddr     string        `koanf:"metrics_addr"`
    LogLevel        string        `koanf:"log_level"`
    DrainTaints     []DrainTaint  `koanf:"drain_taints"`
    RequeueInterval time.Duration `koanf:"requeue_interval"`
    RolloutTimeout  time.Duration `koanf:"rollout_timeout"`
}
```

### node_controller.go

**Struct:**

```go
type NodeReconciler struct {
    client.Client
    Scheme          *runtime.Scheme
    Recorder        record.EventRecorder
    DrainTaints     []DrainTaint  // Configurable list of taints to watch
    RequeueInterval time.Duration // From config
    RolloutTimeout  time.Duration // From config
}

type DrainTaint struct {
    Key    string
    Effect corev1.TaintEffect // Optional: if empty, matches any effect
}
```

**Watches:**
- Primary: `Node` objects
- The reconciler should use a predicate to only enqueue Nodes that have any of the configured drain taints (use an `EventFilter` on Create/Update).

**Constants (annotations — not configurable):**

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
)
```

**Configurable values (from Config struct, loaded by koanf):**

`DrainTaints`, `RequeueInterval`, and `RolloutTimeout` come from the `Config` struct (see main.go section above). The reconciler receives them at construction time.

**Default drain taints (defined in config defaults):**

```go
var DefaultDrainTaints = []DrainTaint{
    {Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
    {Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule},
    {Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule},
}
```

**Key helper functions:**

1. `hasDrainTaint(node *corev1.Node, drainTaints []DrainTaint) bool` — checks if the node has any of the configured drain taints
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
1. Create a Node with a drain taint
2. Create a Deployment with replicas=1 and the opt-in annotation
3. Create a Pod on that Node owned by the Deployment (via a ReplicaSet)
4. Run the reconciler
5. Assert that the Deployment's pod template now has the `restartedAt` annotation
6. Assert that the Node has the `processing` annotation

Test cases:
- Happy path: single Deployment, single pod, triggers restart (test with each default taint)
- Skip: Deployment without opt-in annotation → no restart
- Skip: Deployment with replicas > 1 → no restart
- Skip: Node without any drain taint → no action
- Skip: Node with a taint that is not in the configured list → no action
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
  repository: your-registry/graceful-drain-controller
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

# Drain taints to watch for. Each entry has a key and an optional effect.
# If effect is omitted, the controller matches the taint key with any effect.
drainTaints:
  - key: "karpenter.sh/disrupted"
    effect: "NoSchedule"
  - key: "ToBeDeletedByClusterAutoscaler"
    effect: "NoSchedule"
  - key: "node.kubernetes.io/unschedulable"
    effect: "NoSchedule"
```

## How to Use

### 1. Deploy the controller

```bash
helm install graceful-drain-controller ./deploy/helm/graceful-drain-controller -n kube-system
```

### 2. (Optional) Customize watched taints

To only watch for Karpenter taints:

```yaml
# values-karpenter-only.yaml
drainTaints:
  - key: "karpenter.sh/disrupted"
    effect: "NoSchedule"
```

To add a custom taint:

```yaml
drainTaints:
  - key: "karpenter.sh/disrupted"
    effect: "NoSchedule"
  - key: "ToBeDeletedByClusterAutoscaler"
    effect: "NoSchedule"
  - key: "my-custom-drainer/draining"
    effect: "NoSchedule"
```

### 3. Configure target Deployments

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

### 4. Create a PDB for each target Deployment (MANDATORY)

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

**This PDB is mandatory.** Without it, the autoscaler evicts the pod instantly before the controller can act. The PDB blocks eviction and buys time for the rollout to complete.

## Edge Cases to Handle

1. **Pod with no Deployment owner** (e.g., StatefulSet, bare pod): Skip. This controller only handles Deployments.
2. **Deployment already being deleted**: Skip (check DeletionTimestamp).
3. **Node removed before rollout completes**: The rollout continues normally since the new pod is on a different node. The old pod will be garbage collected. No harm done.
4. **Controller restarts during processing**: The node still has the taint, so the controller will re-reconcile. The `isAlreadyRestarting` check prevents re-triggering. The `processing` and `processing-since` annotations help track state.
5. **Rollout stuck (image pull error, crashloop, etc.)**: The timeout mechanism (RolloutTimeout = 5min) stops the controller from requeueing forever. After timeout, the autoscaler's own drain timeout will eventually force-drain the node.
6. **Autoscaler eviction retry timing**: Autoscalers retry evictions continuously while waiting for the node to drain. As soon as the surge pod is Ready and the PDB is satisfied, the next retry will succeed. There is no manual step needed to "unblock" the autoscaler.
7. **Multiple nodes drained simultaneously**: Each node is reconciled independently. The controller processes each node's pods separately. If two nodes have pods from the same Deployment, the `isAlreadyRestarting` check prevents double-triggering.
8. **Deployment with `maxSurge: 0` or `maxUnavailable: 1`**: The rollout restart will kill the old pod before creating a new one, causing downtime despite the PDB. The controller should log a warning if it detects this misconfiguration.

## Go Dependencies

```
github.com/urfave/cli/v3           v3.6.2   # CLI framework
github.com/knadh/koanf/v2          v2.3.2   # Configuration management
github.com/knadh/koanf/providers/env          # Env var provider for koanf
github.com/knadh/koanf/providers/file         # File provider for koanf
github.com/knadh/koanf/providers/structs      # Struct defaults provider for koanf
github.com/knadh/koanf/parsers/yaml           # YAML parser for koanf
k8s.io/api                         v0.35.1
k8s.io/apimachinery                v0.35.1
k8s.io/client-go                   v0.35.1
sigs.k8s.io/controller-runtime     v0.23.1
```

Target Go version: 1.23+
