# Graceful Drain Controller

A Kubernetes controller that enables **zero-downtime node drains** for singleton (`replicas: 1`) Deployments.

## The Problem

When a node autoscaler (Karpenter, Cluster Autoscaler, or `kubectl drain`) removes a node, it taints the node and evicts pods via the Kubernetes Eviction API. For Deployments running with `replicas: 1`, this creates a dilemma:

- **No PDB**: The pod is evicted instantly. Downtime until the replacement starts elsewhere.
- **PDB with `minAvailable: 1`**: The eviction is blocked. The node can never drain. The autoscaler gives up or force-drains after a timeout.

Neither option gives zero-downtime drains for singleton Deployments.

## How It Works

The controller breaks the PDB deadlock by cooperating with the autoscaler:

```
Autoscaler taints Node X → tries to evict Pod A → PDB blocks it (retries in a loop)

Controller (in parallel):
  → Sees drain taint on Node X
  → Finds Pod A → Deployment D (replicas=1, eligible)
  → Triggers rollout restart on Deployment D
    → K8s creates Pod B on a healthy node (maxSurge=1)
  → Pod B becomes Ready → 2 pods running temporarily
  → PDB satisfied (2 pods, minAvailable=1 → 1 eviction OK)
    → Autoscaler's eviction retry succeeds
      → Pod A evicted, Node X drains
```

The key insight: a rollout restart with `maxSurge: 1` creates a surge pod on a healthy node (the draining node is tainted `NoSchedule`). Once the new pod is Ready, the PDB allows eviction of the old one.

## Supported Autoscalers

The controller watches for configurable drain taints. By default:

| Autoscaler | Taint Key | Effect |
|---|---|---|
| Karpenter | `karpenter.sh/disrupted` | `NoSchedule` |
| Cluster Autoscaler | `ToBeDeletedByClusterAutoscaler` | `NoSchedule` |
| kubectl drain / cordon | `node.kubernetes.io/unschedulable` | `NoSchedule` |

Custom taints can be added via configuration.

## Installation

```bash
helm install graceful-drain-controller ./deploy/helm/graceful-drain-controller -n kube-system
```

## Configuration

### Controller flags

| Flag | Env Var | Default | Description |
|---|---|---|---|
| `--port` | `PORT` | `8081` | Health probe port |
| `--log-level` | `GRACEFUL_DRAIN_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--drain-taint` | `GRACEFUL_DRAIN_DRAIN_TAINTS` | *(all 3 above)* | Comma-separated `key:effect` pairs |
| `--enabled-annotation` | `GRACEFUL_DRAIN_ENABLED_ANNOTATION` | `""` | If set, only handle annotated Deployments |
| `--requeue-interval` | `GRACEFUL_DRAIN_REQUEUE_INTERVAL` | `5s` | Requeue interval during rollout |
| `--rollout-timeout` | `GRACEFUL_DRAIN_ROLLOUT_TIMEOUT` | `5m` | Max time to wait for rollout |

### Scope control

By default, the controller applies to **all** `replicas: 1` Deployments on drained nodes. To restrict it to opt-in workloads only:

```yaml
# values.yaml
enabledAnnotation: "graceful-drain.stonal.com/enabled"
```

Then annotate your Deployments:

```yaml
metadata:
  annotations:
    graceful-drain.stonal.com/enabled: "true"
```

## Deployment Prerequisites

Each target Deployment **must** have:

### 1. Rolling update strategy with surge

```yaml
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1        # Create new pod before killing old one
      maxUnavailable: 0   # Never kill old pod until new one is Ready
```

### 2. A PodDisruptionBudget

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: my-app
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: my-app
```

The PDB is **mandatory**. Without it, the autoscaler evicts the pod instantly before the controller can act.

### 3. A readiness probe

The surge pod must report Ready for the PDB to count it. Ensure your Deployment has a readiness probe configured.

## Development

```bash
make build   # Build binary
make test    # Run tests (requires envtest)
make lint    # Run golangci-lint
make fmt     # Format code
make clean   # Remove binary
```
