# Kubernetes deployment

Sablecourt provides a hardened Kustomize base under `base/` and two exposure overlays:

- `overlays/tailscale`: exposes `sablecourtd` exclusively inside your Tailnet via the Tailscale Kubernetes Operator (`https://sablecourt.<tailnet>.ts.net`).
- `overlays/public`: exposes `sablecourtd` over public HTTPS (`cert-manager` TLS) with `--operator-auth-required=true` (fail-closed bearer token check using `operator_bearer_token` from the mounted secret file).

## Prerequisites

Create the `sablecourt` namespace, mount your manifests ConfigMap, and create the `sablecourt-secrets` Secret containing `secrets.json` (mounted read-only with mode `0400` at `/run/secrets/sablecourt/secrets.json`):

```bash
kubectl apply -f deploy/k8s/base/namespace.yaml

kubectl -n sablecourt create configmap sablecourt-manifests \
  --from-file=local-demo.yaml=examples/local-demo.yaml

kubectl -n sablecourt create secret generic sablecourt-secrets \
  --from-file=secrets.json=/path/to/your-secrets.json
```

Optional: to use Postgres instead of the PVC-backed SQLite database, create `sablecourt-postgres` with key `dsn`:

```bash
kubectl -n sablecourt create secret generic sablecourt-postgres \
  --from-literal=dsn='postgres://user:pass@host:5432/sablecourt?sslmode=require'
```

## Deploy (Tailscale overlay)

Requires the Tailscale Kubernetes Operator installed in the cluster:

```bash
kubectl apply -k deploy/k8s/overlays/tailscale
```

## Deploy (Public HTTPS overlay)

Set `operator_bearer_token` inside `secrets.json` and update `court.example.com` in `overlays/public/ingress-public.yaml`, then apply:

```bash
kubectl apply -k deploy/k8s/overlays/public
```
