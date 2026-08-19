---
title: Preparing your workloads
nav_order: 4
---

# Preparing your workloads

By default every `replicas: 1` Deployment in a watched namespace is protected — there is
nothing to opt into. But protection only *works* if the Deployment can actually produce a
surge pod. Three things have to be true.

## A complete example

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-singleton-app
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1         # required — create the new pod before removing the old one
      maxUnavailable: 0   # required — never remove the old pod first
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
          image: my-app:1.4.2
          ports:
            - containerPort: 8080
          readinessProbe:   # required — this is what "the new pod is up" means
            httpGet:
              path: /healthz
              port: 8080
            periodSeconds: 2
```

That is the whole configuration. No annotations, no PodDisruptionBudget.

## 1. `maxSurge: 1` and `maxUnavailable: 0`

This is the requirement that does the actual work. It is what makes Kubernetes start the new
pod *before* removing the old one.

With `maxSurge: 0`, or `maxUnavailable: 1`, the rollout takes the old pod down first — so the
controller would trigger the very disruption it is trying to prevent. It logs a warning when
it sees either:

```
WARN deployment has maxSurge=0, rollout restart will cause downtime deployment=... namespace=...
```

These are also the defaults you want anyway: `maxUnavailable: 0` is what makes any rollout of
a singleton Deployment non-disruptive, drain or no drain.

## 2. A readiness probe

The webhook releases the eviction when the Deployment reports **two Ready replicas**. Without
a readiness probe, a pod is Ready as soon as its container starts — so the eviction is released
before your process can actually serve traffic, and you get a short disruption anyway.

Make the probe mean "I can serve requests". A short `periodSeconds` shortens the drain, since
the eviction is held for exactly as long as the probe takes to pass.

## 3. Somewhere to put the surge pod

The surge pod needs a node that is not the one being drained, with room for its resource
requests and matching its affinity, node selectors and topology constraints.

This is the most common reason a graceful drain quietly turns into a timeout: the cluster is
being scaled *down*, so spare capacity is exactly what is in short supply. If your nodes come
from an autoscaler, that is usually fine — the pending surge pod triggers a scale-up. If you
run a fixed node pool at high utilisation, plan for the headroom.

Watch out in particular for a `podAntiAffinity` on the app's own label with
`requiredDuringSchedulingIgnoredDuringExecution` — during a graceful drain there are briefly
two pods with that label, and a hard anti-affinity rule will refuse to schedule the second one.
Use `preferredDuringScheduling...` instead.

---

## Do not add a PodDisruptionBudget

{: .warning }
> A PDB with `minAvailable: 1` on a `replicas: 1` Deployment will **defeat this controller's
> timeout escape hatch** and can leave a node permanently undrainable.

This is a change from how the controller worked originally, and it is worth being explicit
about, because "singleton Deployment" and "add a PDB" have been paired advice for years.

Admission webhooks run **before** the eviction handler's disruption-budget check. So the two
are evaluated in series, and the eviction only proceeds if *both* agree:

```
eviction request → [ admission webhooks ] → [ PDB check ] → pod deleted
                     this controller
```

On the happy path that is harmless — once the surge pod is Ready there are 2 replicas, and both
the webhook and a `minAvailable: 1` PDB are satisfied.

It breaks on the unhappy path. When a rollout cannot complete, the controller deliberately
gives up after `rolloutTimeout` and allows the eviction, so the node can drain and the
autoscaler can get on with its job. If a PDB is also in play, that decision is overruled: the
budget still cannot be met with one replica, the eviction is still refused, and you are back to
the deadlock the controller exists to prevent — in precisely the situation where you needed the
way out.

**The webhook is the blocking mechanism now.** It does not need a PDB's help.

### If you want protection for when the controller is down

The one thing a PDB still buys you is coverage for the window where the controller itself is
unavailable — `failurePolicy: Ignore` means evictions pass straight through in that case.

Close that window by running two controller replicas instead:

```yaml
# values.yaml
replicaCount: 2
```

Both serve the webhook; leader election ensures only one reconciles. That gives you the same
coverage without re-introducing an undrainable node.

---

## Opt-in mode

If you would rather protect a specific set of workloads than everything with one replica, set
an annotation key at install time:

```yaml
# values.yaml
enabledAnnotation: "graceful-drain.stonal.com/enabled"
```

Then only Deployments carrying that annotation set to `"true"` are handled; everything else is
evicted normally.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-singleton-app
  annotations:
    graceful-drain.stonal.com/enabled: "true"
```

The key is yours to choose — any annotation name works, so you can reuse one your platform
already sets.

The other scoping lever is [`webhook.namespaceSelector`]({{ '/configuration/#webhook' | relative_url }}),
which decides which namespaces the webhook is called for at all. Prefer it when your boundary
is "these namespaces"; the annotation is for a boundary of "these workloads".
