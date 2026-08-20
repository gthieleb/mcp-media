package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pathArgs mirrors the input schema of example-mcp's download_file tool.
type pathArgs struct {
	Path string `json:"path" jsonschema:"path of the file to download"`
}

// newMCPServer returns an in-process streamable HTTP MCP server exposing a
// single tool (named toolName) that always responds with one TextContent
// item holding toolText. It mirrors the wiring of cmd/example-mcp.
func newMCPServer(t *testing.T, toolName, toolText string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-mcp", Version: "v0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolName,
		Description: "test tool returning fixed text",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ pathArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toolText},
			},
		}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

// newContentServer returns an HTTP server serving body with status 200.
func newContentServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(httpServer.Close)
	return httpServer
}

// testCtx gives each test a generous but bounded deadline.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestRunWithoutURL exercises the probe against a tool that returns a plain
// path (like example-mcp's download_file): listing and calling must succeed,
// the fetch step must be reported as skipped.
func TestRunWithoutURL(t *testing.T) {
	mcpServer := newMCPServer(t, "download_file", "/tmp/hello-world.txt")

	var out bytes.Buffer
	opts := options{
		url:         mcpServer.URL,
		tool:        "download_file",
		path:        "/tmp/hello-world.txt",
		expectBytes: -1,
	}
	if err := run(testCtx(t), opts, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"tool: download_file",
		"text: /tmp/hello-world.txt",
		"fetch: skipped",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q; got:\n%s", want, out.String())
		}
	}
}

// TestRunWithURLFetch exercises the full flow: the tool result contains an
// http URL, which the probe fetches, verifying status and byte length.
func TestRunWithURLFetch(t *testing.T) {
	body := "Hello, World!\n"
	contentServer := newContentServer(t, body)
	mcpServer := newMCPServer(t, "download_file", "downloaded: "+contentServer.URL+"/file.txt")

	var out bytes.Buffer
	opts := options{
		url:         mcpServer.URL,
		tool:        "download_file",
		path:        "/tmp/whatever",
		expectBytes: len(body),
	}
	if err := run(testCtx(t), opts, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"tool: download_file",
		"status: 200",
		"bytes: 14",
		"expect-bytes: 14",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q; got:\n%s", want, out.String())
		}
	}
}

// TestRunExpectBytesMismatch verifies that a wrong -expect-bytes value
// turns the run into a failure.
func TestRunExpectBytesMismatch(t *testing.T) {
	contentServer := newContentServer(t, "Hello, World!\n")
	mcpServer := newMCPServer(t, "download_file", contentServer.URL)

	opts := options{
		url:         mcpServer.URL,
		tool:        "download_file",
		path:        "/tmp/whatever",
		expectBytes: 999,
	}
	if err := run(testCtx(t), opts, &bytes.Buffer{}); err == nil {
		t.Fatal("expected byte-count mismatch error, got nil")
	}
}

// TestRunUnknownTool verifies that calling a tool the server does not
// register is reported as a failure.
func TestRunUnknownTool(t *testing.T) {
	mcpServer := newMCPServer(t, "download_file", "/tmp/hello-world.txt")

	opts := options{
		url:         mcpServer.URL,
		tool:        "does_not_exist",
		path:        "/tmp/whatever",
		expectBytes: -1,
	}
	if err := run(testCtx(t), opts, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}

// TestRunFetchBadStatus verifies that a non-2xx fetch response fails the run.
func TestRunFetchBadStatus(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(contentServer.Close)
	mcpServer := newMCPServer(t, "download_file", contentServer.URL)

	opts := options{
		url:         mcpServer.URL,
		tool:        "download_file",
		path:        "/tmp/whatever",
		expectBytes: -1,
	}
	if err := run(testCtx(t), opts, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for 404 fetch, got nil")
	}
}
