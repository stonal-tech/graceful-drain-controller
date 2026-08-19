---
title: How it works
nav_order: 2
---

# How it works

The controller is a single binary running two cooperating pieces: a **validating admission
webhook** on `pods/eviction`, and a **reconciler** watching Deployments. Neither of them
watches nodes, and neither of them polls — the whole flow is driven by the autoscaler's own
eviction retry loop.

## The eviction dance

![Sequence of the eviction dance]({{ '/assets/img/sequence.svg' | relative_url }})

**Steps 1–5 — the first eviction attempt.**
The autoscaler posts an eviction. The API server calls the webhook, which resolves the pod's
owner (pod → ReplicaSet → Deployment), decides the Deployment is eligible, stamps
`graceful-drain.stonal.com/restarted-at` on it, and answers `429`. The autoscaler treats a
`429` on an eviction as "budget not available right now" and keeps retrying — exactly what it
would do against a PodDisruptionBudget.

**Steps 6–7 — the rollout.**
The reconciler is watching Deployments carrying that annotation. It patches
`kubectl.kubernetes.io/restartedAt` onto the **pod template**, which is what actually triggers
the rollout. This is deliberately split from the webhook: admission handlers should return
fast and be safe to retry, so the webhook only records intent and the reconciler does the work.

**The rollout itself.**
With `maxSurge: 1` and `maxUnavailable: 0`, Kubernetes creates the new pod *before* removing
the old one. The draining node is tainted `NoSchedule`, so the scheduler places the surge pod
elsewhere. Once it passes its readiness probe, the Deployment reports 2 Ready replicas.

**Steps 8–11 — the eviction succeeds.**
On the next retry the webhook sees `ReadyReplicas >= 2` and allows it. The old pod is deleted,
the node finishes draining, and the reconciler clears the tracking annotation.

## Where the state lives

All of it is in one annotation on the Deployment. There is no in-memory state, no CRD, and no
leader-owned queue that matters for correctness.

![Deployment state machine]({{ '/assets/img/lifecycle.svg' | relative_url }})

That means restarting the controller mid-drain costs nothing: the next eviction retry reads
the same annotation back and continues from where it left off.

## The decision table

The webhook allows an eviction — immediately and without side effects — in all of these cases:

| Situation | Why |
|---|---|
| Dry-run request | Admission must not have side effects on dry runs |
| Pod no longer exists | Nothing left to protect |
| Pod has no owning Deployment | StatefulSet, DaemonSet, bare pod — out of scope |
| Deployment has `replicas != 1` | A normal PDB handles this correctly |
| `enabledAnnotation` is set and the Deployment lacks it | Explicitly out of scope |
| Deployment is being deleted | The workload is going away anyway |
| `ReadyReplicas >= 2` | The surge pod is up; disruption budget is satisfied |
| `restarted-at` is older than `rolloutTimeout` | The escape hatch — see below |
| The Deployment could not be resolved, or the annotation patch failed | Never block a drain because of a controller bug |

It denies with `429` in exactly two cases: it has just requested a rollout restart, or a
rollout it requested is still in progress.

There is one deliberate exception to "when in doubt, allow": if the pod itself cannot be read
from the cache, the webhook returns `429` rather than allowing. A cold cache right after
startup would otherwise look identical to "this pod is unprotected", and evicting a singleton
because the controller had not warmed up yet is the one failure worth being cautious about.

## Failure modes, and what happens

**The controller is down or unreachable.**
The webhook is registered with `failurePolicy: Ignore`, so the API server gives up after
`timeoutSeconds` and admits the eviction. You get the behaviour you would have had without
the controller installed — a disruption, not a wedged cluster. If you would rather close that
window, run two replicas rather than switching to `failurePolicy: Fail`.

**The rollout never completes** — bad image, crashloop, failing readiness probe, no room in the
cluster for the surge pod. After `rolloutTimeout` (10 minutes by default) the webhook stops
denying and the drain proceeds. The reconciler clears the annotation and emits a
`GracefulDrainTimeout` warning event. You take the disruption, but the node is not stuck.

**The autoscaler runs out of patience first.**
Every autoscaler has its own drain ceiling — Karpenter's `terminationGracePeriod`, Cluster
Autoscaler's `--max-graceful-termination-sec`. When it expires they stop asking politely and
delete the pod directly, which does not go through the Eviction API and therefore not through
this webhook. Keep `rolloutTimeout` comfortably below that ceiling so the controller is the
one that gives up first, on its own terms.

**The node is drained before the rollout finishes.**
Nothing special happens. The surge pod is on a different node and continues starting up; the
old pod is gone. This is just a normal rollout with an unlucky start.
