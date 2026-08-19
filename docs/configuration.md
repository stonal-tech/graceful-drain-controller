---
title: Configuration reference
nav_order: 5
---

# Configuration reference

## Helm values

### Core

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Controller replicas. Set to `2` to remove the window during an upgrade where evictions bypass the webhook. |
| `image.repository` | `ghcr.io/stonal-tech/graceful-drain-controller` | Controller image. |
| `image.tag` | `""` | Defaults to the chart's `appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Required while the GHCR package is private. |
| `resources` | 50m CPU / 128Mi requested, 256Mi limit | |
| `logLevel` | `info` | `debug`, `info`, `warn`, `error`. JSON output. |
| `port` | `8081` | Health probe port (`/healthz`, `/readyz`). |
| `webhookPort` | `9443` | HTTPS port for the admission webhook. |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Standard scheduling controls. |

### Behaviour

| Value | Default | Description |
|---|---|---|
| `enabledAnnotation` | `""` | If set, only Deployments carrying this annotation set to `"true"` are protected. Empty means every `replicas: 1` Deployment is. |
| `rolloutTimeout` | `10m` | How long the webhook keeps denying before it gives up and lets the drain proceed. |
| `requeueInterval` | `10s` | How often the reconciler re-checks a rollout in progress. |

`rolloutTimeout` is the important one. It must be **longer** than a worst-case pod start
(image pull on a cold node, slow readiness probe) and **shorter** than your autoscaler's own
drain ceiling — Karpenter's `terminationGracePeriod`, Cluster Autoscaler's
`--max-graceful-termination-sec`. Sitting between the two means the controller gives up on its
own terms, emitting a `GracefulDrainTimeout` event you can alert on, rather than being cut off
mid-flight by a forceful pod deletion.

### Webhook

| Value | Default | Description |
|---|---|---|
| `webhook.failurePolicy` | `Ignore` | `Ignore`: an unreachable controller means evictions proceed normally. `Fail`: an unreachable controller blocks every eviction in scope. |
| `webhook.timeoutSeconds` | `5` | How long the API server waits for the webhook. |
| `webhook.namespaceSelector` | excludes `kube-system` | Which namespaces the webhook is called for. |

{: .warning }
> Think hard before setting `failurePolicy: Fail`. It puts this controller in the critical path
> of every eviction in every selected namespace — including the evictions the cluster needs in
> order to recover. `Ignore` degrades to "as if not installed"; `Fail` degrades to "nothing can
> be evicted". Two replicas is the better way to buy availability.

The default `namespaceSelector` excludes `kube-system`, which is also where the chart expects
to be installed. That is deliberate: it keeps the controller out of the control plane's way,
and stops it from intercepting the eviction of its own pod.

### TLS

| Value | Default | Description |
|---|---|---|
| `certManager.enabled` | `true` | Create a self-signed cert-manager `Issuer` + `Certificate` and let cert-manager inject the CA bundle. |
| `tls.existingSecret` | `""` | Required when `certManager.enabled` is `false`. A TLS secret in the release namespace with `tls.crt` / `tls.key`. |
| `tls.caBundle` | `""` | Base64-encoded CA for the API server to trust. Leave empty if something else injects it. |

See [bring your own certificate]({{ '/installation/#bring-your-own-certificate' | relative_url }}).

## Command-line flags

Every flag has an environment variable equivalent. The Helm chart sets them as flags.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--port` | `PORT` | `8081` | Health probe port |
| `--webhook-port` | `GRACEFUL_DRAIN_WEBHOOK_PORT` | `9443` | Webhook HTTPS port |
| `--cert-dir` | `GRACEFUL_DRAIN_CERT_DIR` | `""` | Directory holding `tls.crt` / `tls.key` |
| `--log-level` | `GRACEFUL_DRAIN_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--enabled-annotation` | `GRACEFUL_DRAIN_ENABLED_ANNOTATION` | `""` | Restrict to Deployments with this annotation |
| `--requeue-interval` | `GRACEFUL_DRAIN_REQUEUE_INTERVAL` | `5s` | Rollout re-check interval |
| `--rollout-timeout` | `GRACEFUL_DRAIN_ROLLOUT_TIMEOUT` | `5m` | Give-up deadline |

{: .note }
> The binary's defaults for `--requeue-interval` (`5s`) and `--rollout-timeout` (`5m`) differ
> from the chart's (`10s` and `10m`). The chart values are the ones that apply to a Helm
> install.

## Annotations

| Annotation | Set on | Set by | Meaning |
|---|---|---|---|
| `graceful-drain.stonal.com/restarted-at` | Deployment metadata | the webhook | A graceful drain is in progress. RFC 3339 timestamp, used for the timeout check. Removed by the reconciler when the rollout completes or times out. |
| `kubectl.kubernetes.io/restartedAt` | Deployment pod template | the reconciler | The standard `kubectl rollout restart` annotation. Patching it is what starts the rollout. |
| *(your key)* | Deployment metadata | you | Opt-in marker, when `enabledAnnotation` is configured. |

## RBAC

The controller runs with a ClusterRole granting:

| Resource | Verbs | Why |
|---|---|---|
| `pods` | get, list, watch | Resolve the pod named in the eviction |
| `replicasets` | get, list, watch | Walk pod → ReplicaSet → Deployment |
| `deployments` | get, list, watch, patch | Read rollout status, set annotations |
| `events` | create, patch | Report progress on the Deployment |
| `leases` | get, create, update | Leader election |

Note what is absent: no write access to nodes, no `delete` on pods, no cluster-admin. The
controller never removes a workload — it only ever asks the Deployment controller to roll one.
