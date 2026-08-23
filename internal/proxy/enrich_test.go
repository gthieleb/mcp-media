package proxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerSaveTool adds an upstream-style tool that reports a sandbox path
// in its structured content (the enrichment target shape).
func registerSaveTool(server *mcp.Server, name string, structured map[string]any, isError bool) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: "saves a report"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		res := &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "saved"}},
			StructuredContent: structured,
		}
		if isError {
			res.IsError = true
			return res, nil, nil
		}
		return res, nil, nil
	})
}

type enrichFixture struct {
	ctx     context.Context
	session *mcp.ClientSession
	mint    *recordingMintHandler
}

func newEnrichFixture(t *testing.T, toolMatch string) *enrichFixture {
	t.Helper()
	ctx := context.Background()

	mh := &recordingMintHandler{respBody: mintBody(42, "text/plain")}
	mintServer := httptest.NewServer(mh)
	t.Cleanup(mintServer.Close)

	mc, err := NewMintClient(mintServer.URL, "test-token", nil)
	if err != nil {
		t.Fatalf("NewMintClient: %v", err)
	}
	enricher, err := NewEnricher(mc, toolMatch, testLogger())
	if err != nil {
		t.Fatalf("NewEnricher: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	server.AddReceivingMiddleware(enricher.Middleware())
	registerSaveTool(server, "save_report", map[string]any{"file_path": "/tmp/out/report.txt"}, false)
	registerSaveTool(server, "list_things", map[string]any{"count": 3}, false)
	registerSaveTool(server, "save_broken", map[string]any{"unrelated": true}, false)
	registerSaveTool(server, "save_failed", map[string]any{"file_path": "/tmp/out/x.txt"}, true)

	httpSrv := serveMCP(t, server)
	session := connectClient(t, ctx, httpSrv.URL)
	return &enrichFixture{ctx: ctx, session: session, mint: mh}
}

func TestEnrichmentAddsURLToMatchingTool(t *testing.T) {
	fx := newEnrichFixture(t, `^save_`)

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name:      "save_report",
		Arguments: map[string]any{},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v isError=%v", err, res.IsError)
	}

	sc := structuredMap(t, res)
	for key, want := range map[string]any{
		"url":        signedURL,
		"mime_type":  "text/plain",
		"size_bytes": float64(42),
	} {
		if sc[key] != want {
			t.Errorf("structured[%q] = %v, want %v", key, sc[key], want)
		}
	}
	textsGot := texts(res)
	if len(textsGot) != 2 || textsGot[1] != signedURL {
		t.Errorf("expected original text plus URL TextContent appended, got %q", textsGot)
	}
	_, auth, body := fx.mint.snapshot()
	if auth != "Bearer test-token" || body["path"] != "/tmp/out/report.txt" {
		t.Errorf("mint request mismatch: auth=%q body=%v", auth, body)
	}
	if body["disposition"] != "attachment" {
		t.Errorf("disposition = %v, want attachment", body["disposition"])
	}
}

func TestEnrichmentLeavesNonMatchingResponsesUnchanged(t *testing.T) {
	fx := newEnrichFixture(t, `^save_`)

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: "list_things"})
	if err != nil || res.IsError {
		t.Fatalf("call failed: %v", err)
	}
	sc := structuredMap(t, res)
	if _, ok := sc["url"]; ok {
		t.Error("non-matching tool was enriched")
	}
	if hits, _, _ := fx.mint.snapshot(); hits != 0 {
		t.Errorf("mint called for non-matching tool (%d times)", hits)
	}
}

func TestEnrichmentSkipsIsErrorAndMissingPath(t *testing.T) {
	fx := newEnrichFixture(t, `.*`)

	// IsError result: untouched even though a file_path field exists.
	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: "save_failed"})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError passthrough")
	}
	if sc := structuredMap(t, res); len(sc) != 1 || sc["file_path"] != "/tmp/out/x.txt" {
		t.Errorf("error result modified: %v", sc)
	}

	// No path field: unchanged.
	res, err = fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: "save_broken"})
	if err != nil || res.IsError {
		t.Fatalf("call failed: %v", err)
	}
	sc := structuredMap(t, res)
	if _, ok := sc["url"]; ok {
		t.Error("response without path field was enriched")
	}
	if hits, _, _ := fx.mint.snapshot(); hits != 0 {
		t.Errorf("mint called despite missing path field (%d times)", hits)
	}
}

func TestEnrichmentDisabledWithoutMatchRegex(t *testing.T) {
	fx := newEnrichFixture(t, "")

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: "save_report"})
	if err != nil || res.IsError {
		t.Fatalf("call failed: %v", err)
	}
	if sc := structuredMap(t, res); len(texts(res)) != 1 {
		t.Errorf("content changed while enrichment disabled: %v", texts(res))
	} else if _, ok := sc["url"]; ok {
		t.Error("enriched although disabled")
	}
	if hits, _, _ := fx.mint.snapshot(); hits != 0 {
		t.Error("mint called although disabled")
	}
}

func TestEnrichmentSurvivesMintFailure(t *testing.T) {
	fx := newEnrichFixture(t, `^save_`)
	fx.mint.status = 500
	fx.mint.respBody = `{"error":"boom"}`

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: "save_report"})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if res.IsError {
		t.Fatal("upstream success must stay successful when enrichment fails")
	}
	sc := structuredMap(t, res)
	if _, ok := sc["url"]; ok {
		t.Error("partial enrichment after mint failure")
	}
	if got := texts(res); len(got) != 1 || got[0] != "saved" {
		t.Errorf("original response altered: %q", got)
	}
	if !strings.Contains(strings.Join(texts(res), ""), "saved") {
		t.Error("content lost")
	}
}

func TestNewEnricherValidation(t *testing.T) {
	mc, err := NewMintClient("http://localhost:8091", "tok", nil)
	if err != nil {
		t.Fatalf("NewMintClient: %v", err)
	}
	if _, err := NewEnricher(nil, "^x", nil); err == nil {
		t.Error("nil mint client must be rejected")
	}
	if _, err := NewEnricher(mc, "([bad", nil); err == nil {
		t.Error("invalid regex must be rejected")
	}
	if e, err := NewEnricher(mc, "", nil); err != nil || e.re != nil {
		t.Error("empty match must disable enrichment")
	}
}
