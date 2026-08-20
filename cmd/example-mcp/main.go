// Command example-mcp is the reference MCP server of the mcp-media project.
//
// It serves a single tool, download_file, over the streamable HTTP transport
// on :7777. On startup it writes the well-known file /tmp/hello-world.txt
// ("Hello, World!\n"), which is served by the media sidecar in later tasks.
// The tool always returns that well-known path, regardless of the requested
// path.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// helloFilePath is the well-known file returned by the download_file
	// tool and served by the media sidecar in later tasks.
	helloFilePath = "/tmp/hello-world.txt"
	// helloFileContent is the exact content written to helloFilePath on
	// startup.
	helloFileContent = "Hello, World!\n"

	// listenAddr is the address the streamable HTTP server binds to.
	listenAddr = ":7777"
)

// downloadFileArgs is the input schema of the download_file tool.
type downloadFileArgs struct {
	Path string `json:"path" jsonschema:"path of the file to download"`
}

// downloadFile implements the download_file tool. The requested path is
// ignored for content purposes: the tool always returns the well-known
// hello file path.
func downloadFile(_ context.Context, _ *mcp.CallToolRequest, _ downloadFileArgs) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: helloFilePath},
		},
	}, nil, nil
}

// newServer builds the MCP server with all tools registered.
func newServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "example-mcp",
		Version: "0.1.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "download_file",
		Description: "Download a file; returns the path of the downloaded file",
	}, downloadFile)
	return server
}

// writeHelloFile writes the well-known hello file to path.
func writeHelloFile(path string) error {
	if err := os.WriteFile(path, []byte(helloFileContent), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func main() {
	if err := writeHelloFile(helloFilePath); err != nil {
		log.Fatalf("example-mcp: %v", err)
	}

	server := newServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("example-mcp: streamable HTTP MCP server listening on %s", listenAddr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("example-mcp: server failed: %v", err)
		}
	case <-ctx.Done():
		log.Printf("example-mcp: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("example-mcp: graceful shutdown failed: %v", err)
		}
	}
}
