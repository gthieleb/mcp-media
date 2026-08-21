package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// helloFilePath mirrors the well-known path returned by the reference
// example-mcp server (cmd/example-mcp).
const helloFilePath = "/tmp/hello-world.txt"

// downloadFileArgs mirrors the input schema of example-mcp's download_file
// tool.
type downloadFileArgs struct {
	Path string `json:"path" jsonschema:"path of the file to download"`
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// addDownloadFileTool registers the example-mcp download_file tool on server:
// it ignores the requested path and always returns the well-known hello file
// path.
func addDownloadFileTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "download_file",
		Description: "Download a file; returns the path of the downloaded file",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ downloadFileArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: helloFilePath}},
		}, nil, nil
	})
}

// serveMCP exposes server over the streamable HTTP transport on a test HTTP
// server.
func serveMCP(t *testing.T, server *mcp.Server) *httptest.Server {
	t.Helper()
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	t.Cleanup(httpServer.Close)
	return httpServer
}

// connectClient connects an MCP client to the streamable HTTP endpoint url.
// Server-initiated messages (SSE) are disabled: the tests only need
// request/response on the downstream side.
func connectClient(t *testing.T, ctx context.Context, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client Connect %q: %v", url, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// findTool returns the named tool from a tools/list result, or nil.
func findTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string) *mcp.Tool {
	t.Helper()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

// callText calls the named tool with the given arguments and returns the
// single text content item of the result.
func callText(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool %s: tool returned error result: %+v", name, res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("CallTool %s: expected 1 content item, got %d", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool %s: expected *mcp.TextContent, got %T", name, res.Content[0])
	}
	return tc.Text
}

// TestMirrorAndForward exercises the full proxy path in-process: an
// example-mcp-style upstream server, an Upstream mirroring it onto a fresh
// downstream server, and a downstream client listing and calling the
// mirrored download_file tool.
func TestMirrorAndForward(t *testing.T) {
	ctx := context.Background()

	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "example-mcp", Version: "0.1.0"}, nil)
	addDownloadFileTool(upstreamServer)
	upstreamHTTP := serveMCP(t, upstreamServer)

	downstreamServer := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	up := NewUpstream(upstreamHTTP.URL, &UpstreamOptions{Logger: testLogger()})
	if err := up.Connect(ctx, downstreamServer); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { up.Close() })
	downstreamHTTP := serveMCP(t, downstreamServer)

	// The downstream tools/list must contain download_file with the same
	// description and input schema as the upstream listing.
	upstreamSession := connectClient(t, ctx, upstreamHTTP.URL)
	want := findTool(t, ctx, upstreamSession, "download_file")
	if want == nil {
		t.Fatal("download_file not found in upstream tools/list")
	}
	downstreamSession := connectClient(t, ctx, downstreamHTTP.URL)
	got := findTool(t, ctx, downstreamSession, "download_file")
	if got == nil {
		t.Fatal("download_file not found in downstream tools/list")
	}
	if got.Description != want.Description {
		t.Errorf("description not passed through: want %q, got %q", want.Description, got.Description)
	}
	if diff := schemaDiff(want.InputSchema, got.InputSchema); diff != "" {
		t.Errorf("input schema not passed through: %s", diff)
	}

	// A tools/call via the downstream server must be forwarded upstream with
	// the arguments intact, returning the well-known hello file path.
	text := callText(t, ctx, downstreamSession, "download_file", map[string]any{"path": "/tmp/whatever"})
	if text != helloFilePath {
		t.Errorf("forwarded call: expected text %q, got %q", helloFilePath, text)
	}
}

// schemaDiff compares two schemas by their canonical JSON map representation
// and returns a non-empty description if they differ.
func schemaDiff(a, b any) string {
	norm := func(v any) map[string]any {
		raw, err := json.Marshal(v)
		if err != nil {
			return map[string]any{"marshal-error": err.Error()}
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return map[string]any{"unmarshal-error": err.Error()}
		}
		return m
	}
	am, bm := norm(a), norm(b)
	if !reflect.DeepEqual(am, bm) {
		aj, _ := json.Marshal(am)
		bj, _ := json.Marshal(bm)
		return "want " + string(aj) + ", got " + string(bj)
	}
	return ""
}

// TestSyncToolsPagination verifies that SyncTools follows tools/list
// pagination cursors: an upstream with PageSize 1 and two tools must yield
// both tools downstream.
func TestSyncToolsPagination(t *testing.T) {
	ctx := context.Background()

	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "example-mcp", Version: "0.1.0"},
		&mcp.ServerOptions{PageSize: 1})
	addDownloadFileTool(upstreamServer)
	mcp.AddTool(upstreamServer, &mcp.Tool{
		Name:        "echo_path",
		Description: "Echo the requested path",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args downloadFileArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: args.Path}},
		}, nil, nil
	})
	upstreamHTTP := serveMCP(t, upstreamServer)

	downstreamServer := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	up := NewUpstream(upstreamHTTP.URL, &UpstreamOptions{Logger: testLogger()})
	if err := up.Connect(ctx, downstreamServer); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { up.Close() })
	downstreamHTTP := serveMCP(t, downstreamServer)

	downstreamSession := connectClient(t, ctx, downstreamHTTP.URL)
	for _, name := range []string{"download_file", "echo_path"} {
		if findTool(t, ctx, downstreamSession, name) == nil {
			t.Errorf("tool %q missing downstream after paginated sync", name)
		}
	}

	// Both tools must forward.
	if text := callText(t, ctx, downstreamSession, "echo_path", map[string]any{"path": "/x"}); text != "/x" {
		t.Errorf("echo_path: expected %q, got %q", "/x", text)
	}
}

// TestSyncToolsDuplicateGuard verifies that an upstream tool whose name is
// reserved by the downstream server is skipped: the downstream keeps its own
// handler and no tool is replaced.
func TestSyncToolsDuplicateGuard(t *testing.T) {
	ctx := context.Background()

	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "example-mcp", Version: "0.1.0"}, nil)
	addDownloadFileTool(upstreamServer)
	upstreamHTTP := serveMCP(t, upstreamServer)

	// The downstream server owns its own download_file tool.
	downstreamServer := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	mcp.AddTool(downstreamServer, &mcp.Tool{
		Name:        "download_file",
		Description: "downstream's own download_file",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ downloadFileArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "own"}},
		}, nil, nil
	})

	up := NewUpstream(upstreamHTTP.URL, &UpstreamOptions{
		Logger:        testLogger(),
		ReservedTools: []string{"download_file"},
	})
	if err := up.Connect(ctx, downstreamServer); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { up.Close() })
	downstreamHTTP := serveMCP(t, downstreamServer)

	downstreamSession := connectClient(t, ctx, downstreamHTTP.URL)
	if text := callText(t, ctx, downstreamSession, "download_file", map[string]any{"path": "/tmp/whatever"}); text != "own" {
		t.Errorf("downstream handler was replaced: expected %q, got %q", "own", text)
	}
	tool := findTool(t, ctx, downstreamSession, "download_file")
	if tool.Description != "downstream's own download_file" {
		t.Errorf("downstream tool description was replaced: got %q", tool.Description)
	}
	up.mu.Lock()
	mirrored := len(up.mirrored)
	up.mu.Unlock()
	if mirrored != 0 {
		t.Errorf("expected no mirrored tools, got %d", mirrored)
	}
}

// TestToolListChangedResync verifies that a tool-list-changed notification
// from the upstream server triggers a re-sync: tools added upstream appear
// downstream, and tools removed upstream disappear downstream.
func TestToolListChangedResync(t *testing.T) {
	ctx := context.Background()

	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "example-mcp", Version: "0.1.0"}, nil)
	addDownloadFileTool(upstreamServer)
	upstreamHTTP := serveMCP(t, upstreamServer)

	downstreamServer := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	up := NewUpstream(upstreamHTTP.URL, &UpstreamOptions{Logger: testLogger()})
	if err := up.Connect(ctx, downstreamServer); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { up.Close() })
	downstreamHTTP := serveMCP(t, downstreamServer)
	downstreamSession := connectClient(t, ctx, downstreamHTTP.URL)

	if findTool(t, ctx, downstreamSession, "extra_tool") != nil {
		t.Fatal("extra_tool unexpectedly present before it was added upstream")
	}

	// Add a tool upstream: the notification must trigger a re-sync that
	// mirrors the new tool.
	mcp.AddTool(upstreamServer, &mcp.Tool{
		Name:        "extra_tool",
		Description: "added after the initial sync",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "extra"}},
		}, nil, nil
	})
	waitForTool(t, ctx, downstreamSession, "extra_tool", true)
	if text := callText(t, ctx, downstreamSession, "extra_tool", map[string]any{}); text != "extra" {
		t.Errorf("extra_tool: expected %q, got %q", "extra", text)
	}

	// Remove it upstream: the re-sync must remove the mirror downstream.
	upstreamServer.RemoveTools("extra_tool")
	waitForTool(t, ctx, downstreamSession, "extra_tool", false)
}

// waitForTool polls the downstream tools/list until the named tool is
// present (want=true) or absent (want=false), or the deadline passes.
func waitForTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		found := false
		for _, tool := range res.Tools {
			if tool.Name == name {
				found = true
				break
			}
		}
		if found == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for tool %q presence=%v downstream", name, want)
}

// TestConnectUnreachableUpstream verifies that a connection failure at
// startup is returned as an error.
func TestConnectUnreachableUpstream(t *testing.T) {
	ctx := context.Background()
	// A closed httptest server yields a guaranteed-refused endpoint.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	up := NewUpstream(dead.URL, &UpstreamOptions{Logger: testLogger()})
	server := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	if err := up.Connect(ctx, server); err == nil {
		t.Fatal("Connect to unreachable upstream: expected error, got nil")
	}
}

// TestNormalizeObjectSchema covers the schema normalization used before
// re-registering upstream tools downstream.
func TestNormalizeObjectSchema(t *testing.T) {
	tests := []struct {
		name    string
		in      any
		want    map[string]any
		wantErr bool
	}{
		{name: "nil becomes empty object schema", in: nil, want: map[string]any{"type": "object"}},
		{
			name: "missing type is filled in", in: map[string]any{"properties": map[string]any{}},
			want: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			name: "object schema passes through", in: map[string]any{"type": "object", "title": "x"},
			want: map[string]any{"type": "object", "title": "x"},
		},
		{name: "non-object type is an error", in: map[string]any{"type": "string"}, wantErr: true},
		{name: "non-object JSON is an error", in: "nope", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeObjectSchema(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got schema %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}
