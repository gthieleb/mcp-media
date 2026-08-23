package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pathKeys are the structured-content fields scanned for sandbox paths.
var pathKeys = []string{"file_path", "path", "filename"}

// Enricher reactively adds signed download URLs to tool responses: when a
// mirrored tool's structured content carries a path field (file_path, path,
// filename), the Enricher mints a URL for it and merges {url, mime_type,
// size_bytes} into the response plus the URL as TextContent. Responses are
// never modified when the tool does not match TOOL_MATCH, errored, or no
// path field is present.
type Enricher struct {
	mc     *MintClient
	re     *regexp.Regexp // nil → enrichment disabled
	logger *slog.Logger
}

// NewEnricher compiles the TOOL_MATCH regex. An empty match string disables
// enrichment entirely.
func NewEnricher(mc *MintClient, match string, logger *slog.Logger) (*Enricher, error) {
	if mc == nil {
		return nil, fmt.Errorf("proxy: enricher requires a mint client")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var re *regexp.Regexp
	if match != "" {
		var err error
		re, err = regexp.Compile(match)
		if err != nil {
			return nil, fmt.Errorf("proxy: invalid TOOL_MATCH regex %q: %w", match, err)
		}
	}
	return &Enricher{mc: mc, re: re, logger: logger}, nil
}

// Middleware wraps tool-call handling so successful responses of matching
// tools gain signed-URL enrichments. Register via Server.AddReceivingMiddleware.
func (e *Enricher) Middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil || res == nil || e.re == nil {
				return res, err
			}
			ctr, ok := req.(*mcp.CallToolRequest)
			if !ok {
				return res, err
			}
			result, ok := res.(*mcp.CallToolResult)
			if !ok {
				return res, err
			}
			e.enrich(ctx, ctr.Params.Name, result)
			return result, nil
		}
	}
}

// enrich mutates result in place with minted URL data; any failure leaves
// the upstream response untouched (enrichment is best-effort).
func (e *Enricher) enrich(ctx context.Context, toolName string, result *mcp.CallToolResult) {
	if result.IsError || !e.re.MatchString(toolName) {
		return
	}
	pathValue, ok := extractPathField(result.StructuredContent)
	if !ok {
		return
	}
	mr, err := e.mc.Mint(ctx, pathValue, "attachment", 0)
	if err != nil {
		e.logger.Warn("enrichment mint failed", "tool", toolName, "path", pathValue, "err", err)
		return
	}

	structured := ensureMap(result.StructuredContent)
	structured["url"] = mr.URL
	structured["mime_type"] = mr.MimeType
	structured["size_bytes"] = mr.SizeBytes
	result.StructuredContent = structured
	result.Content = append(result.Content, &mcp.TextContent{Text: mr.URL})
}

// extractPathField finds the first non-empty path field in a structured
// content object.
func extractPathField(sc any) (string, bool) {
	m := ensureMap(sc)
	for _, k := range pathKeys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// ensureMap normalizes structured content to a mutable map (handlers set
// maps; wire-decoded values may arrive as json.RawMessage).
func ensureMap(sc any) map[string]any {
	switch v := sc.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any{}
		}
		return v
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err == nil && m != nil {
			return m
		}
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err == nil && m != nil {
			return m
		}
	case nil:
		return map[string]any{}
	}
	return map[string]any{}
}
