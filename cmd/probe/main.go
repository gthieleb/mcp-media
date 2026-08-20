// Command probe is the smoke-test MCP client of the mcp-media project.
//
// It connects to a streamable HTTP MCP endpoint via the Go SDK, lists the
// available tools, calls one tool (default: download_file) with a path
// argument, scans the returned text content for an http(s) URL and, if one
// is found, fetches it with a plain HTTP GET, verifying the response status
// and byte length. If the tool result contains no URL, the fetch step is
// skipped and the run still counts as success (used against the bare
// example-mcp server, which returns a plain path).
//
// Exit code is 0 on success and 1 on any failure. Step logs go to stderr,
// grep-able results (tool names, status, bytes) go to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// urlRE extracts the first http(s) URL from tool text output.
var urlRE = regexp.MustCompile(`https?://[^\s"'<>]+`)

// options configures a single probe run.
type options struct {
	url         string // streamable HTTP endpoint of the MCP server
	tool        string // name of the tool to call
	path        string // value of the tool's "path" argument
	expectBytes int    // expected byte length of the fetched body; <0 disables the check
}

// run executes the probe flow against opts.url and writes results to out.
// It returns a non-nil error if any step failed.
func run(ctx context.Context, opts options, out io.Writer) error {
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0.1.0"}, nil)
	log.Printf("connecting to %s", opts.url)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             opts.url,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	for _, tool := range tools.Tools {
		fmt.Fprintf(out, "tool: %s\n", tool.Name)
	}

	log.Printf("calling tool %q with path %q", opts.tool, opts.path)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      opts.tool,
		Arguments: map[string]any{"path": opts.path},
	})
	if err != nil {
		return fmt.Errorf("tools/call %s: %w", opts.tool, err)
	}
	if res.IsError {
		return fmt.Errorf("tools/call %s: tool returned error result: %+v", opts.tool, res.Content)
	}

	var texts []string
	for _, content := range res.Content {
		tc, ok := content.(*mcp.TextContent)
		if !ok {
			log.Printf("skipping non-text content of type %T", content)
			continue
		}
		texts = append(texts, tc.Text)
		fmt.Fprintf(out, "text: %s\n", tc.Text)
	}

	var fetchURL string
	for _, text := range texts {
		if u := urlRE.FindString(text); u != "" {
			fetchURL = u
			break
		}
	}
	if fetchURL == "" {
		fmt.Fprintf(out, "fetch: skipped (no URL in tool result)\n")
		return nil
	}

	log.Printf("fetching %s", fetchURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return fmt.Errorf("build fetch request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", fetchURL, err)
	}
	defer resp.Body.Close()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("read body of %s: %w", fetchURL, err)
	}
	fmt.Fprintf(out, "fetch: %s\n", fetchURL)
	fmt.Fprintf(out, "status: %d\n", resp.StatusCode)
	fmt.Fprintf(out, "bytes: %d\n", n)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("fetch %s: unexpected status %d", fetchURL, resp.StatusCode)
	}
	if opts.expectBytes >= 0 {
		fmt.Fprintf(out, "expect-bytes: %d\n", opts.expectBytes)
		if n != int64(opts.expectBytes) {
			return fmt.Errorf("fetch %s: expected %d bytes, got %d", fetchURL, opts.expectBytes, n)
		}
	}
	return nil
}

func main() {
	log.SetPrefix("probe: ")

	var opts options
	var showUsage bool
	flag.StringVar(&opts.url, "url", "", "streamable HTTP endpoint of the MCP server (required)")
	flag.StringVar(&opts.tool, "tool", "download_file", "name of the tool to call")
	flag.StringVar(&opts.path, "path", "/tmp/hello-world.txt", "value of the tool's path argument")
	flag.IntVar(&opts.expectBytes, "expect-bytes", -1, "expected byte length of the fetched body (negative = no check)")
	flag.BoolVar(&showUsage, "h", false, "show usage")
	flag.Parse()

	if showUsage || opts.url == "" {
		if opts.url == "" && !showUsage {
			fmt.Fprintln(os.Stderr, "probe: -url is required")
		}
		flag.Usage()
		if showUsage {
			os.Exit(0)
		}
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	if err := run(ctx, opts, os.Stdout); err != nil {
		log.Printf("FAIL: %v", err)
		fmt.Fprintf(os.Stdout, "result: FAIL (%v)\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "result: OK")
	log.Printf("done in %s", time.Since(start).Round(time.Millisecond))
}
