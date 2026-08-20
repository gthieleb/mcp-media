package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDownloadFileHandler verifies that the tool handler returns the
// well-known hello file path regardless of the requested path.
func TestDownloadFileHandler(t *testing.T) {
	res, _, err := downloadFile(context.Background(), nil, downloadFileArgs{Path: "/does/not/matter.bin"})
	if err != nil {
		t.Fatalf("downloadFile returned error: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
	}
	if tc.Text != helloFilePath {
		t.Errorf("expected text %q, got %q", helloFilePath, tc.Text)
	}
}

// TestWriteHelloFile verifies that the startup file is written with the exact
// expected content.
func TestWriteHelloFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello-world.txt")
	if err := writeHelloFile(path); err != nil {
		t.Fatalf("writeHelloFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(data); got != helloFileContent {
		t.Errorf("expected content %q, got %q", helloFileContent, got)
	}
	if helloFileContent != "Hello, World!\n" {
		t.Errorf("helloFileContent must be exactly %q, got %q", "Hello, World!\n", helloFileContent)
	}
}

// TestDownloadFileOverStreamableHTTP exercises the full stack: the streamable
// HTTP handler serving an in-process MCP client that lists tools and calls
// download_file.
func TestDownloadFileOverStreamableHTTP(t *testing.T) {
	server := newServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	// The server must expose download_file with a "path" input property.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found bool
	for _, tool := range tools.Tools {
		if tool.Name == "download_file" {
			found = true
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("unmarshal input schema: %v", err)
			}
			prop, ok := schema.Properties["path"]
			if !ok || prop.Type != "string" {
				t.Errorf("download_file input schema missing string \"path\" property: %s", raw)
			}
		}
	}
	if !found {
		t.Fatalf("download_file tool not registered; got tools: %+v", tools.Tools)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "download_file",
		Arguments: map[string]any{"path": "/tmp/whatever"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error result: %+v", res)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
	}
	if tc.Text != helloFilePath {
		t.Errorf("expected text %q, got %q", helloFilePath, tc.Text)
	}
}
