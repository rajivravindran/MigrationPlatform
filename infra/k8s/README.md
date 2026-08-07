# Kubernetes / Helm

This directory contains the Helm chart used to deploy the Migration Platform to
a Kubernetes cluster. The chart under [`helm/`](./helm) wraps our four
application services (`api`, `orchestrator-go`, `transform-worker`, `web`) and
brings in Postgres, Redis, MinIO, and Temporal as subchart dependencies.

## Install

```bash
helm dep update infra/k8s/helm
helm upgrade --install migration infra/k8s/helm \
  --namespace migration --create-namespace \
  -f infra/k8s/helm/values.yaml \
  -f infra/k8s/helm/values-dev.yaml \
  --set image.tag=$(git rev-parse --short HEAD)
```

For production use `values-prod.yaml` instead of `values-dev.yaml`.

## Layout

- `helm/Chart.yaml`, `values.yaml`, `values-dev.yaml`, `values-prod.yaml`
- `helm/templates/` — Deployments, Services, HPAs, PDBs, NetworkPolicies, Ingress, ConfigMap/Secret, migration Job (pre-install hook), backup CronJob, ServiceMonitor (if Prometheus Operator present)

## Cutting over from Compose

See [`docs/operations.md`](../../docs/operations.md) for restore, scale, rolling
upgrade, and backup verification runbooks.
