---
title: FAQ
nav_order: 7
---

# FAQ

### Which autoscalers does this work with?

Any mechanism that evicts through the Kubernetes Eviction API: Karpenter, Cluster Autoscaler,
`kubectl drain`, node upgrade tooling, custom drain controllers. The controller never reads
nodes or taints, so there is nothing autoscaler-specific to configure — it only ever sees an
eviction request for a pod.

### Do I need a PodDisruptionBudget?

No — and you should not add one for a single-replica Deployment. The webhook is the blocking
mechanism now, and a `minAvailable: 1` PDB will override the controller's timeout escape hatch
and can leave the node undrainable.
[The full explanation]({{ '/workloads/#do-not-add-a-poddisruptionbudget' | relative_url }}).

### Does it work for StatefulSets?

No. Only Deployments. A pod whose owner chain does not end at a Deployment is allowed through
untouched.

StatefulSets need a different mechanism anyway: the ordinal identity means a "surge pod" cannot
exist alongside the pod it replaces, which is the entire trick this controller relies on.

### What about Deployments with 2 or more replicas?

Ignored. A standard PodDisruptionBudget already handles them correctly — the eviction of one
pod is blocked until the others are healthy, which is exactly what you want and needs no help.
This controller exists only for the `replicas: 1` case that a PDB cannot express.

### What happens if the controller is down?

Evictions proceed as if it were not installed. The webhook is registered with
`failurePolicy: Ignore`, so an unreachable controller means a disruption, not a stuck cluster.
Run `replicaCount: 2` if you want to close that window.

### Does this make drains slower?

Yes, by roughly the time it takes your pod to become Ready — typically a few seconds to a
minute per protected Deployment. That is the trade being made: a slower drain in exchange for
no disruption. It is bounded by `rolloutTimeout`.

### Does it cost extra capacity?

One extra pod per protected Deployment, for the duration of the drain. If the cluster cannot
fit it, the surge pod stays Pending and the drain falls back to the timeout — so on a tightly
packed fixed-size node pool, plan for the headroom.

### Why a rollout restart rather than scaling to 2 and back?

A rollout restart is a single idempotent annotation patch that Kubernetes already knows how to
drive, and it converges on its own if the controller dies halfway. Scaling would mean writing
`spec.replicas`, remembering the original value somewhere durable, and restoring it afterwards
— which fights with HPAs and GitOps controllers that consider `replicas` theirs.

The rollout also replaces the pod, which is what you wanted anyway: the old one is on a node
that is about to disappear.

### Will my GitOps controller fight this?

The controller writes two annotations: one on the Deployment metadata and one on the pod
template. Argo CD and Flux both treat `kubectl.kubernetes.io/restartedAt` as expected drift by
default, since `kubectl rollout restart` writes the same field. If you run a strict
self-heal / prune-on-drift setup, exclude both annotations:

```
graceful-drain.stonal.com/restarted-at
kubectl.kubernetes.io/restartedAt
```

### Does it work with `kubectl drain --disable-eviction`?

No. That flag deletes pods directly instead of evicting them, which bypasses admission on
`pods/eviction` — and bypasses PodDisruptionBudgets too. The same applies to an autoscaler's
forceful termination once its own grace period has expired.

### Can two nodes drain at once?

Yes. Evictions are handled independently, and the state is per-Deployment, so nothing
serialises across nodes. A single-replica Deployment only has one pod, so it can only ever be
affected by one node's drain at a time.

### Why deny with `429` rather than a plain "denied"?

Because `429 Too Many Requests` is what the Eviction API returns when a PodDisruptionBudget
blocks an eviction, and every drain implementation already treats it as "retry, this is
temporary". A plain denial would read as a permanent failure, and some drain loops would give
up or escalate to a forced delete. Reusing the status code is what lets the controller work
with autoscalers that know nothing about it.

### How do I turn it off for one workload?

Switch to [opt-in mode]({{ '/workloads/#opt-in-mode' | relative_url }}) and annotate only the
Deployments you want protected. There is no per-workload opt-*out* in always-on mode — scale
the Deployment beyond one replica, or move it to a namespace outside the
`webhook.namespaceSelector`.
