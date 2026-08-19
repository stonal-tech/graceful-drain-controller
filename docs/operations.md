---
title: Operations
nav_order: 6
---

# Operations

## Events

The controller reports on the Deployment being protected, so `kubectl describe` on the
workload tells the whole story:

```bash
kubectl describe deployment my-singleton-app
```

| Reason | Type | Meaning |
|---|---|---|
| `GracefulDrainTriggered` | Normal | A rollout restart was started for a drain |
| `GracefulDrainCompleted` | Normal | The rollout finished; the eviction can proceed |
| `GracefulDrainTimeout` | Warning | The rollout did not finish in `rolloutTimeout`; the drain was allowed to proceed anyway |

`GracefulDrainTimeout` is the one to alert on. Every occurrence is a disruption that the
controller was meant to prevent and could not.

## Logs

JSON on stdout, via `log/slog`. The lines that matter:

```
INFO  requested rollout restart, denying eviction   deployment=... namespace=... pod=...
INFO  triggering rollout restart                    deployment=... namespace=...
INFO  rollout in progress, denying eviction         deployment=... namespace=...
INFO  rollout complete, allowing eviction           deployment=... namespace=...
INFO  rollout complete, removing tracking annotation deployment=... namespace=...
WARN  rollout timeout exceeded, allowing eviction   deployment=... namespace=...
WARN  deployment has maxSurge=0, ...                deployment=... namespace=...
```

A healthy drain produces the first four in order, a few seconds to a minute apart.

Set `logLevel: debug` to also see each requeue while a rollout is in progress.

## Watching a drain happen

```bash
# what the controller is doing
kubectl logs -n kube-system -l app.kubernetes.io/name=graceful-drain-controller -f

# replica counts, live
kubectl get deployment my-singleton-app -w

# where the pods are
kubectl get pods -l app=my-singleton-app -o wide -w
```

You should see `READY` go `1/1 → 1/2 → 2/2 → 1/1`, with the second pod on a different node.

---

## Troubleshooting

### The node is stuck draining

Look at the surge pod first — the drain is waiting on it.

```bash
kubectl get pods -l app=my-singleton-app -o wide
kubectl describe pod <the-new-pending-pod>
```

- **Pending, `FailedScheduling`** — no room for the surge pod. Check resource requests against
  remaining capacity, and check for a hard `podAntiAffinity` on the app's own label, which
  cannot tolerate two pods existing at once.
- **`ImagePullBackOff` / `CrashLoopBackOff`** — the rollout will never complete. The drain
  unblocks at `rolloutTimeout`.
- **Running but not Ready** — the readiness probe is failing or is slower than you think.

If there is no new pod at all, the rollout was never triggered. Check the tracking annotation:

```bash
kubectl get deployment my-singleton-app \
  -o jsonpath='{.metadata.annotations.graceful-drain\.stonal\.com/restarted-at}'
```

Present but no new pod means the reconciler is not acting — check the controller logs and that
its leader-election lease is held. Absent means the webhook never fired; see below.

### Evictions are not being intercepted at all

Pods are evicted instantly, no `429`, nothing in the logs.

1. **Is the webhook registered?**

   ```bash
   kubectl get validatingwebhookconfiguration graceful-drain-controller
   ```

2. **Is the namespace in scope?** The default `namespaceSelector` excludes `kube-system`.
   Namespaces are matched on the `kubernetes.io/metadata.name` label, which the API server
   maintains automatically from Kubernetes 1.21.

3. **Is the CA bundle populated?**

   ```bash
   kubectl get validatingwebhookconfiguration graceful-drain-controller \
     -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | head -c 40
   ```

   Empty means cert-manager has not injected it. With `failurePolicy: Ignore`, TLS failures are
   silent from the client's point of view — the API server logs them, and evictions sail
   through. This is the most common cause of "it does nothing".

4. **Is the Deployment eligible?** `replicas` must be exactly 1, and if `enabledAnnotation` is
   configured the Deployment must carry it set to `"true"`. Turn on `logLevel: debug` and watch
   an eviction to see which check bailed out.

### `429` responses right after the controller starts

```
graceful drain: cache not ready, retry later
```

The webhook fails closed when it cannot read the pod, so that a cold cache is never mistaken
for an unprotected workload. It clears within a few seconds of startup, and the autoscaler's
retry loop absorbs it.

### Drains got slower

Expected. Each protected pod now holds its eviction for as long as its replacement takes to
become Ready. A node with several singleton Deployments pays that cost for each one, since the
evictions are handled independently.

If it is too slow, the lever is pod startup time — image size, probe `periodSeconds`,
`initialDelaySeconds` — not the controller.
