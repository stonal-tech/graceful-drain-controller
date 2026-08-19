---
title: Overview
nav_order: 1
---

# graceful-drain-controller

Zero-disruption node drains for single-replica Kubernetes Deployments.
{: .fs-6 .fw-300 }

[Install it]({{ '/installation/' | relative_url }}){: .btn .btn-primary .mr-2 }
[How it works]({{ '/how-it-works/' | relative_url }}){: .btn }

---

## The problem

Cluster autoscalers remove nodes all day long. Karpenter consolidates, Cluster Autoscaler
scales down, an operator runs `kubectl drain` — and every one of them ends the same way:
taint the node, then evict its pods through the Kubernetes Eviction API.

For a Deployment running `replicas: 1`, that leaves you two options, and both are bad.

![Two bad options for a single-replica Deployment]({{ '/assets/img/problem.svg' | relative_url }})

Without a PodDisruptionBudget, the eviction is accepted the moment it arrives. Your only
pod disappears, and the service is down until a replacement is scheduled, pulled, started
and Ready somewhere else — anywhere from a few seconds to a few minutes.

With a PDB of `minAvailable: 1`, the eviction is refused instead. But a single-replica
Deployment can never satisfy that budget while its only pod is running, so the refusal
never lifts. The node stays half-drained until the autoscaler's patience runs out and it
force-deletes the pod — which is the first outcome again, just later and less predictably.

The honest fix is to run two replicas. That is not always possible: leader-elected
controllers, singleton workers, licensed software, and anything holding an exclusive lock
are genuinely single-instance.

## The idea

The disruption exists because the old pod goes away *before* the new one arrives. So make
the eviction wait — not forever, just long enough for a replacement to be up.

![What the controller does]({{ '/assets/img/solution.svg' | relative_url }})

`graceful-drain-controller` registers a validating admission webhook on `pods/eviction`.
When an eviction arrives for the only pod of a `replicas: 1` Deployment, it:

1. **Denies the eviction** with `429 Too Many Requests` — the status code every autoscaler
   already knows how to retry.
2. **Marks the Deployment for a rollout restart**, which schedules a surge pod. The draining
   node is tainted `NoSchedule`, so the surge pod lands on a healthy node.
3. **Keeps denying** until that pod passes its readiness probe.
4. **Allows the eviction** on the next retry, once two replicas are Ready.

The old pod is evicted, the node drains, the autoscaler terminates it. The replica count
went `1 → 2 → 1` and never touched zero.

## What you get

- Works with **Karpenter**, **Cluster Autoscaler**, `kubectl drain`, and anything else that
  goes through the Eviction API — the controller never looks at nodes or taints, so there
  is nothing to configure per autoscaler.
- **No PodDisruptionBudget required.** The webhook is the blocking mechanism.
  ([And a `minAvailable: 1` PDB is actively harmful here]({{ '/workloads/#do-not-add-a-poddisruptionbudget' | relative_url }}).)
- **No annotations required** by default. Every `replicas: 1` Deployment is protected;
  opt-in mode is available if you want a smaller blast radius.
- **Fails open.** If the controller is unavailable, evictions behave exactly as if it were
  never installed. A broken controller cannot wedge your cluster.
- **Bounded.** A rollout that never completes gives up after `rolloutTimeout` and lets the
  drain proceed, rather than blocking the node indefinitely.

## What it does not do

- **StatefulSets, DaemonSets and bare pods** are ignored. Only Deployments are handled.
- **`replicas: 2` and above** are ignored — a normal PDB already solves that case correctly.
- **Direct pod deletion** is not intercepted. `kubectl drain --disable-eviction`, and an
  autoscaler's forceful termination once its own grace period expires, both bypass the
  Eviction API and therefore this controller.
- **It cannot create capacity.** If there is nowhere to put the surge pod, the rollout stalls
  and the drain falls back to the timeout.

## Next steps

- [How it works]({{ '/how-it-works/' | relative_url }}) — the full sequence, the state machine, and the failure modes.
- [Installation]({{ '/installation/' | relative_url }}) — prerequisites and Helm install.
- [Preparing your workloads]({{ '/workloads/' | relative_url }}) — what a protected Deployment must look like.
- [Configuration reference]({{ '/configuration/' | relative_url }}) — every flag and Helm value.
- [Operations]({{ '/operations/' | relative_url }}) — events, logs, troubleshooting.
