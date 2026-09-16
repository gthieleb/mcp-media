# E2E fixtures

- `fake-tailscale-ingressclass.yaml` — fake IngressClass so the Reconciler's
  tailscale fallback path resolves in kind.
- `full-mode-deployment.yaml` — example-mcp upstream + proxy (full mode) +
  sidecar in one pod; enrichment target.
- `standalone-deployment.yaml` — proxy in standalone mode (agent-pod egress).

Secrets (`media-signing`, `media-internal-token`) and the
`media-injection=enabled` namespace label are applied by the e2e harness
(workflow / local runner) — never committed.
