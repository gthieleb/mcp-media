# mcp-media

Generic MCP media egress: a sidecar + proxy + Kubernetes controller that exposes
media files trapped inside isolated MCP-server pods via short-lived HMAC-signed
download URLs — without forking each MCP server.

## Status

🚧 Bootstrap (Wave 0). See [docs/plans/2026-08-20-mcp-media.md](docs/plans/2026-08-20-mcp-media.md)
for the full implementation plan.

## Components (planned)

| Binary | Role |
|---|---|
| `media-sidecar` | Data plane: file server + mint API, mounts the media volume read-only |
| `mcp-media-proxy` | Control plane: terminates MCP (official Go SDK), injects `stream_media`/`download_file` and enriches existing tool results via middleware |
| `example-mcp` | Reference MCP server returning `/tmp/hello-world.txt` — test fixture |
| `probe` | Smoke-test client for E2E |
| `media-controller` | kubebuilder controller: MutatingWebhook (sidecar/proxy injection) + Reconciler (Service + inherited Ingress) |

## License

MIT