# k8s-webhook-operator

An HTTP-driven Kubernetes operator that can cordon, drain, and uncordon nodes when it receives REST calls. It runs as a controller-runtime manager (no CRDs yet) and exposes a small API on port 8080.

## Prerequisites
- Go 1.25+ (for local builds/tests)
- Docker or Podman (for container builds/push)
- kubectl
- helm (for Helm-based install)
- Access to a Kubernetes cluster (k3d/Rancher Desktop locally, or any cluster for Helm)

## Build without Make (Docker or Podman)
```sh
scripts/build-and-push.sh -i <registry>/k8s-webhook-operator:dev --push
# use Podman
TOOL=podman scripts/build-and-push.sh -i <registry>/k8s-webhook-operator:dev --push
```

## What it does
- Exposes `POST /cordon`, `POST /drain`, and `POST /uncordon` endpoints.
- Uses the in-cluster RBAC-enabled client to patch node schedulability and to drain via the upstream `kubectl` drain helper.
- Ships with a Service and Traefik Ingress so you can reach it easily in k3d/Rancher Desktop.

## Deploy locally (k3d/Rancher Desktop + Traefik)
1. Build and push an image (adjust registry/tag as needed):
   ```sh
   make docker-build docker-push IMG=<registry>/k8s-webhook-operator:dev
   # or with Podman
   CONTAINER_TOOL=podman make docker-build docker-push IMG=<registry>/k8s-webhook-operator:dev
   ```
2. Deploy the manifests (includes RBAC, Service, Ingress) into `k8s-webhook-operator-system`:
   ```sh
   make deploy IMG=<registry>/k8s-webhook-operator:dev
   ```
3. Point a host at your k3d load balancer (Traefik typically listens on 80/443):
   ```sh
   echo "127.0.0.1 node-actions.local" | sudo tee -a /etc/hosts
   ```
   The provided Ingress (`config/default/ingress.yaml`) routes all traffic for `node-actions.local` to the operator Service.

To run the controller locally against your kubeconfig instead of deploying, use:
```sh
make install   # installs RBAC only (no CRDs present today)
make run
```

## Deploy to any cluster (Helm)
```sh
helm install k8s-webhook-operator charts/k8s-webhook-operator \
  --namespace k8s-webhook-operator-system --create-namespace \
  --set image.repository=<registry>/k8s-webhook-operator \
  --set image.tag=dev
```

Ingress options (enable only if your platform uses an ingress controller):
```sh
--set ingress.enabled=true \
--set ingress.hosts[0].host=node-actions.local
```

Access options:
- Port-forward: `kubectl -n k8s-webhook-operator-system port-forward svc/k8s-webhook-operator-api 8080:80`
- Ingress (if enabled): update your host mappings as appropriate for your ingress/LB.

## API
All endpoints expect `Content-Type: application/json` and a POST body.

### /cordon
```json
{ "node": "k3d-k3s-default-server-0" }
```

### /uncordon
```json
{ "node": "k3d-k3s-default-server-0" }
```

### /drain
```json
{
  "node": "k3d-k3s-default-server-0",
  "force": false,
  "deleteEmptyDirData": false,
  "ignoreDaemonSets": true,
  "gracePeriodSeconds": -1,
  "timeoutSeconds": 300
}
```
`ignoreDaemonSets` defaults to `true`, `gracePeriodSeconds` defaults to `-1` (use the pod spec), and `timeoutSeconds` defaults to 300 if omitted.

Example curl through Traefik:
```sh
curl -X POST http://node-actions.local/drain \
  -H "Content-Type: application/json" \
  -d '{"node":"k3d-k3s-default-server-0","force":true,"deleteEmptyDirData":true}'
```

## Notes and next steps
- No authentication is wired in; keep the Ingress closed or add auth at Traefik if you expose it broadly.
- RBAC is scoped to nodes and pod evictions only (see `config/rbac/role.yaml`).
- Future work: expose a small CRD to audit/track actions, add request signing/auth, and tighten timeouts/retries.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
