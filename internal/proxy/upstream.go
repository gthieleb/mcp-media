// Package proxy implements the mcp-media MCP proxy: it mirrors the tools of
// an upstream MCP server onto its own (downstream) MCP server and forwards
// tool calls, so that downstream clients see the upstream tools as-is.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// syncTimeout bounds a single tools/list network round trip during
// SyncTools. Without it a hung upstream would wedge the sync section
// (and every goroutine blocked on it) forever.
const syncTimeout = 30 * time.Second

// Upstream manages one persistent MCP client session to an upstream MCP
// server and mirrors the upstream tool list onto a downstream MCP server.
//
// A single *mcp.ClientSession is shared by all mirrored tool handlers;
// ClientSession is safe for concurrent use. Should multiplexing issues with
// concurrent downstream callers ever appear, the documented fallback is one
// upstream session per downstream session.
//
// Connection lifecycle: Connect establishes the session and performs the
// initial tool sync; a connection failure at that point is returned to the
// caller (which decides whether it is fatal). There is no automatic
// re-connect: if the upstream dies mid-run, mirrored tool calls fail with the
// session error, and a re-sync is only attempted when a
// tool-list-changed notification arrives (which requires a live session).
//
// Locking discipline: mu guards session/server/mirrored and is only ever
// held for fast in-memory sections — never across network round trips, so
// mirrored tool handlers are not blocked by a slow or hung tools/list.
// listMu serializes whole SyncTools runs (startup sync vs.
// tool-list-changed re-sync); it may be held across the network listing.
type Upstream struct {
	endpoint string
	logger   *slog.Logger
	client   *mcp.Client

	mu       sync.Mutex // guards session, server and mirrored (fast sections only)
	session  *mcp.ClientSession
	server   *mcp.Server         // downstream server to re-sync onto
	mirrored map[string]struct{} // tool names this Upstream registered downstream
	reserved map[string]struct{} // tool names owned by the downstream server itself

	listMu sync.Mutex // serializes SyncTools (may be held across the listing)
}

// UpstreamOptions configures an Upstream.
type UpstreamOptions struct {
	// Logger receives proxy activity logs. Nil selects slog.Default().
	Logger *slog.Logger
	// ReservedTools lists tool names owned by the downstream server itself
	// (e.g. the generic stream_media/download_file tools). Upstream tools
	// with these names are skipped with a warning and never replace the
	// downstream handlers. The SDK's (*mcp.Server).AddTool silently replaces
	// same-name tools and offers no way to enumerate registered tools, so
	// collisions can only be avoided via this list.
	ReservedTools []string
}

// NewUpstream returns an Upstream mirroring the MCP server at endpoint
// (e.g. "http://localhost:7777"). Connect must be called before SyncTools.
func NewUpstream(endpoint string, opts *UpstreamOptions) *Upstream {
	u := &Upstream{
		endpoint: endpoint,
		logger:   slog.Default(),
		mirrored: make(map[string]struct{}),
		reserved: make(map[string]struct{}),
	}
	if opts != nil {
		if opts.Logger != nil {
			u.logger = opts.Logger
		}
		for _, name := range opts.ReservedTools {
			u.reserved[name] = struct{}{}
		}
	}
	u.client = mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-media-proxy",
		Version: "0.1.0",
	}, &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			u.resync(ctx)
		},
	})
	return u
}

// Connect establishes the persistent session to the upstream server and
// performs the initial tool sync onto server. The upstream transport keeps
// the standalone SSE stream enabled so that tool-list-changed notifications
// are received and trigger a re-sync.
func (u *Upstream) Connect(ctx context.Context, server *mcp.Server) error {
	session, err := u.client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: u.endpoint,
	}, nil)
	if err != nil {
		return fmt.Errorf("proxy: connect upstream %q: %w", u.endpoint, err)
	}
	u.mu.Lock()
	u.session = session
	u.server = server
	u.mu.Unlock()
	if err := u.SyncTools(ctx, server); err != nil {
		_ = session.Close()
		// Reset the session so a partially-failed Connect leaves no stale
		// state behind: the proxy must behave as "not connected".
		u.mu.Lock()
		if u.session == session {
			u.session = nil
		}
		u.mu.Unlock()
		return err
	}
	return nil
}

// Close terminates the upstream session. It is idempotent.
func (u *Upstream) Close() error {
	u.mu.Lock()
	session := u.session
	u.session = nil
	u.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

// SyncTools lists all tools of the upstream server (following pagination
// cursors via the session's Tools iterator) and mirrors them onto server:
// new or changed upstream tools are (re-)registered with forwarding
// handlers, and mirrored tools that disappeared upstream are removed again
// via (*mcp.Server).RemoveTools.
//
// SyncTools is safe for concurrent use (startup sync and
// tool-list-changed handler may race); calls are serialized via listMu.
// The network listing runs WITHOUT holding mu (only fast in-memory
// mutations are locked), so a slow upstream never blocks mirrored tool
// handlers. The listing is bounded by syncTimeout.
func (u *Upstream) SyncTools(ctx context.Context, server *mcp.Server) error {
	u.listMu.Lock()
	defer u.listMu.Unlock()

	u.mu.Lock()
	session := u.session
	u.mu.Unlock()
	if session == nil {
		return errors.New("proxy: upstream not connected")
	}

	listCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	var tools []*mcp.Tool
	for tool, err := range session.Tools(listCtx, nil) {
		if err != nil {
			return fmt.Errorf("proxy: list tools from %q: %w", u.endpoint, err)
		}
		tools = append(tools, tool)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if _, dup := seen[tool.Name]; dup {
			u.logger.Warn("proxy: upstream listed tool twice, skipping duplicate", "tool", tool.Name)
			continue
		}
		seen[tool.Name] = struct{}{}
		if _, ok := u.reserved[tool.Name]; ok {
			u.logger.Warn("proxy: upstream tool collides with a downstream tool, skipping",
				"tool", tool.Name)
			continue
		}
		dt, err := downstreamTool(tool, u.logger)
		if err != nil {
			u.logger.Warn("proxy: cannot mirror upstream tool, skipping",
				"tool", tool.Name, "error", err)
			continue
		}
		server.AddTool(dt, u.forward(tool.Name))
		u.mirrored[tool.Name] = struct{}{}
	}

	// Remove mirrored tools that no longer exist upstream.
	for name := range u.mirrored {
		if _, ok := seen[name]; !ok {
			server.RemoveTools(name)
			delete(u.mirrored, name)
			u.logger.Info("proxy: removed mirrored tool that disappeared upstream", "tool", name)
		}
	}
	return nil
}

// resync re-runs SyncTools after a tool-list-changed notification from the
// upstream server. Errors are logged; the previous mirror stays in place.
func (u *Upstream) resync(ctx context.Context) {
	u.mu.Lock()
	server := u.server
	u.mu.Unlock()
	if server == nil {
		return
	}
	u.logger.Info("proxy: upstream tool list changed, re-syncing")
	if err := u.SyncTools(ctx, server); err != nil {
		u.logger.Warn("proxy: re-sync after tool list change failed", "error", err)
	}
}

// forward returns a tool handler that forwards calls to the upstream tool
// name, passing the raw JSON arguments through verbatim. The session is
// looked up at call time, so a re-established session is picked up
// automatically.
func (u *Upstream) forward(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		u.mu.Lock()
		session := u.session
		u.mu.Unlock()
		if session == nil {
			return nil, fmt.Errorf("proxy: upstream %q is not connected", u.endpoint)
		}
		params := &mcp.CallToolParams{Name: name}
		// req.Params is a *mcp.CallToolParamsRaw: Arguments is the raw JSON
		// received from the downstream client. Forward it verbatim.
		if len(req.Params.Arguments) > 0 {
			params.Arguments = json.RawMessage(req.Params.Arguments)
		}
		return session.CallTool(ctx, params)
	}
}

// downstreamTool builds the tool registered on the downstream server from an
// upstream tool listing. Name, Description, Title, Annotations, Icons and
// Meta are copied verbatim; the schemas are normalized into fresh
// map[string]any values guaranteed to carry "type": "object", which
// (*mcp.Server).AddTool requires (it panics otherwise). An unmirrorable
// input schema is an error (the tool is skipped); a bad output schema only
// drops that optional metadata — the tool is kept.
func downstreamTool(tool *mcp.Tool, logger *slog.Logger) (*mcp.Tool, error) {
	in, err := normalizeObjectSchema(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	dt := &mcp.Tool{
		Meta:        tool.Meta,
		Annotations: tool.Annotations,
		Description: tool.Description,
		InputSchema: in,
		Name:        tool.Name,
		Title:       tool.Title,
		Icons:       tool.Icons,
	}
	if tool.OutputSchema != nil {
		out, err := normalizeObjectSchema(tool.OutputSchema)
		if err != nil {
			// The output schema is optional metadata; drop it rather than
			// losing the whole tool.
			logger.Warn("proxy: dropping unmirrorable output schema",
				"tool", tool.Name, "error", err)
		} else {
			dt.OutputSchema = out
		}
	}
	return dt, nil
}

// normalizeObjectSchema converts a schema decoded from the wire (typically a
// map[string]any) into a fresh map carrying "type": "object". A nil schema
// becomes the empty object schema; a schema without an explicit "type" gets
// "object" filled in; anything that is not a JSON object schema is an error.
func normalizeObjectSchema(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object"}, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("schema is not a JSON object: %w", err)
	}
	if m == nil { // schema marshaled to JSON null
		return map[string]any{"type": "object"}, nil
	}
	typ, ok := m["type"]
	if !ok {
		m["type"] = "object"
		return m, nil
	}
	if typ != "object" {
		return nil, fmt.Errorf(`schema type must be "object", got %v`, typ)
	}
	return m, nil
}
