---
title: Installation
nav_order: 3
---

# Installation

## Prerequisites

**Kubernetes 1.21+.** The webhook's default `namespaceSelector` matches on the
`kubernetes.io/metadata.name` label, which the API server adds automatically from 1.21.

**cert-manager**, or a serving certificate of your own. The webhook is an HTTPS endpoint, so
the API server needs to trust it. By default the chart creates a self-signed cert-manager
`Issuer` and `Certificate` and lets cert-manager inject the CA bundle into the
`ValidatingWebhookConfiguration`. If you do not run cert-manager, see
[bring your own certificate](#bring-your-own-certificate) below.

**An autoscaler that uses the Eviction API.** Karpenter, Cluster Autoscaler and `kubectl drain`
all do. `kubectl drain --disable-eviction` does not.

## Install

The chart is not published to a registry yet, so install it from a checkout:

```bash
git clone https://github.com/stonal-tech/graceful-drain-controller.git
cd graceful-drain-controller

helm install graceful-drain-controller \
  ./deploy/helm/graceful-drain-controller \
  --namespace kube-system
```

Installing into `kube-system` is deliberate: the default `namespaceSelector` excludes that
namespace, so the controller never intercepts evictions of control-plane pods — or of its own.

{: .warning }
> The container image `ghcr.io/stonal-tech/graceful-drain-controller` is currently a **private**
> GHCR package. Until it is made public you need an image pull secret:
>
> ```bash
> kubectl create secret docker-registry ghcr-credentials \
>   --namespace kube-system \
>   --docker-server=ghcr.io \
>   --docker-username="$GITHUB_USER" \
>   --docker-password="$GITHUB_TOKEN"
> ```
>
> then install with `--set imagePullSecrets[0].name=ghcr-credentials`.

## Verify

The controller should be Running and Ready:

```bash
kubectl get pods -n kube-system -l app.kubernetes.io/name=graceful-drain-controller
```

The webhook should be registered with a populated `caBundle`:

```bash
kubectl get validatingwebhookconfiguration graceful-drain-controller \
  -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | head -c 40
```

An empty result means cert-manager has not injected the CA yet — check that cert-manager is
running and that the `Certificate` in the release namespace is Ready.

Then try it for real. Pick a node running a single-replica Deployment and cordon-drain it:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
```

`kubectl drain` will report `error when evicting pod ... graceful drain: triggered rollout
restart, retry later` and keep retrying. That message is the controller working, not a failure.
Within a minute or so the surge pod becomes Ready elsewhere and the drain completes.

## Bring your own certificate

If you do not run cert-manager, disable the chart's certificate resources and point it at a
TLS secret you manage yourself. The secret must live in the release namespace, contain
`tls.crt` and `tls.key`, and be valid for
`<release-name>.<namespace>.svc` and `<release-name>.<namespace>.svc.cluster.local`.

```yaml
certManager:
  enabled: false

tls:
  existingSecret: graceful-drain-controller-tls
  # base64-encoded CA bundle for the API server to trust.
  # Leave empty if something else injects it into the webhook configuration.
  caBundle: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t...
```

## Upgrading

```bash
helm upgrade graceful-drain-controller \
  ./deploy/helm/graceful-drain-controller \
  --namespace kube-system
```

With a single replica there is a short window during the rolling upgrade where the webhook is
unreachable and evictions pass straight through. If drains are running at the same time, set
`replicaCount: 2` — leader election means only one replica reconciles, but both serve the
webhook.

## Uninstall

```bash
helm uninstall graceful-drain-controller --namespace kube-system
```

This removes the `ValidatingWebhookConfiguration` along with everything else, so evictions
immediately go back to their default behaviour. Any Deployment left mid-drain keeps its
`graceful-drain.stonal.com/restarted-at` annotation; it is inert, but you can clean it up with:

```bash
kubectl annotate deployment --all --all-namespaces graceful-drain.stonal.com/restarted-at-
```
