package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// InlineMaxBytesDefault caps inline media payloads at 100 KiB.
const InlineMaxBytesDefault = 102400

// MediaToolOptions configures the generic media tools.
type MediaToolOptions struct {
	// FetchBaseURL optionally replaces scheme+host of minted URLs for the
	// server-side inline fetch (e.g. "http://localhost:8090"). Signatures do
	// not cover the host, so swapped URLs stay valid. Empty = fetch as-is.
	FetchBaseURL string
	// InlineMaxBytes is the maximum payload size eligible for inlining
	// (0 selects InlineMaxBytesDefault).
	InlineMaxBytes int64
	// HTTPClient is used for the inline fetch (nil selects a default).
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// mediaArgs is the shared argument schema of stream_media and download_file.
type mediaArgs struct {
	Path       string `json:"path"`
	Mode       string `json:"mode,omitempty"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// RegisterMediaTools registers the two generic media tools on server:
//
//   - stream_media(path, mode?, ttl_seconds?): disposition "inline",
//     default mode "inline"
//   - download_file(path, mode?, ttl_seconds?): disposition "attachment",
//     default mode "url"
//
// Both return structuredContent {url, mime_type, size_bytes, filename,
// expires_at, inline_included} plus the URL as TextContent. When mode is
// "inline" or "both" and the file is image/* or audio/* within
// InlineMaxBytes, the bytes are additionally attached as ImageContent or
// AudioContent. Mint failures surface as IsError results.
func RegisterMediaTools(server *mcp.Server, mc *MintClient, opts MediaToolOptions) {
	if opts.InlineMaxBytes <= 0 {
		opts.InlineMaxBytes = InlineMaxBytesDefault
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "stream_media",
		Description: "Stream a file from the sandbox filesystem as inline media. Returns a short-lived signed URL, and embeds the bytes directly when they are an image/audio within the inline size limit.",
	}, newMediaToolHandler(mc, opts, "inline", "inline"))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "download_file",
		Description: "Download a file from the sandbox filesystem. Returns a short-lived signed download URL.",
	}, newMediaToolHandler(mc, opts, "attachment", "url"))
}

func newMediaToolHandler(mc *MintClient, opts MediaToolOptions, disposition, defaultMode string) mcp.ToolHandlerFor[mediaArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args mediaArgs) (*mcp.CallToolResult, any, error) {
		mode := args.Mode
		if mode == "" {
			mode = defaultMode
		}
		switch mode {
		case "url", "inline", "both":
		default:
			return errorResult(fmt.Sprintf("invalid mode %q: must be one of url, inline, both", args.Mode)), nil, nil
		}

		mr, err := mc.Mint(ctx, args.Path, disposition, args.TTLSeconds)
		if err != nil {
			opts.Logger.Error("mint failed", "disposition", disposition, "path", args.Path, "err", err)
			return errorResult(err.Error()), nil, nil
		}

		structured := map[string]any{
			"url":             mr.URL,
			"mime_type":       mr.MimeType,
			"size_bytes":      mr.SizeBytes,
			"filename":        path.Base(args.Path),
			"expires_at":      mr.ExpiresAt.UTC().Format(time.RFC3339),
			"inline_included": false,
		}
		contents := []mcp.Content{&mcp.TextContent{Text: mr.URL}}

		if mode == "inline" || mode == "both" {
			includeInline(ctx, mr, structured, &contents, opts)
		}

		return &mcp.CallToolResult{
			Content:           contents,
			StructuredContent: structured,
		}, nil, nil
	}
}

// includeInline attempts to attach the minted file as inline content,
// downgrading to an explanatory note on any ineligibility or failure.
func includeInline(ctx context.Context, mr *MintResponse, structured map[string]any, contents *[]mcp.Content, opts MediaToolOptions) {
	note := func(format string, args ...any) {
		*contents = append(*contents, &mcp.TextContent{Text: fmt.Sprintf(format, args...)})
	}

	if !strings.HasPrefix(mr.MimeType, "image/") && !strings.HasPrefix(mr.MimeType, "audio/") {
		structured["inline_included"] = false
		note("mime_type %q is not inline-eligible (image/* or audio/* only)", mr.MimeType)
		return
	}
	if mr.SizeBytes > opts.InlineMaxBytes {
		structured["inline_included"] = false
		note("file too large for inline inclusion (%d > %d bytes); use the signed URL", mr.SizeBytes, opts.InlineMaxBytes)
		return
	}

	data, err := fetchInline(ctx, mr.URL, opts)
	if err != nil {
		opts.Logger.Warn("inline fetch failed", "err", err)
		structured["inline_included"] = false
		note("inline fetch failed (%v); use the signed URL", err)
		return
	}

	if strings.HasPrefix(mr.MimeType, "image/") {
		*contents = append(*contents, &mcp.ImageContent{Data: data, MIMEType: mr.MimeType})
	} else {
		*contents = append(*contents, &mcp.AudioContent{Data: data, MIMEType: mr.MimeType})
	}
	structured["inline_included"] = true
}

// fetchInline downloads the minted file (base-swapped when FetchBaseURL is
// set), enforcing the inline size limit while reading.
func fetchInline(ctx context.Context, mintedURL string, opts MediaToolOptions) ([]byte, error) {
	fetchURL, err := swapBase(mintedURL, opts.FetchBaseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve fetch URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build fetch request: %w", err)
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, opts.InlineMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > opts.InlineMaxBytes {
		return nil, fmt.Errorf("body exceeds inline limit (%d bytes)", opts.InlineMaxBytes)
	}
	return data, nil
}

// swapBase replaces scheme and host of rawURL with those of baseURL,
// keeping path and query (signatures cover neither host nor query params).
func swapBase(rawURL, baseURL string) (string, error) {
	if baseURL == "" {
		return rawURL, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse minted URL: %w", err)
	}
	b, err := url.Parse(baseURL)
	if err != nil || b.Host == "" || (b.Scheme != "http" && b.Scheme != "https") {
		return "", fmt.Errorf("invalid fetch base URL")
	}
	u.Scheme = b.Scheme
	u.Host = b.Host
	return u.String(), nil
}

// errorResult builds an IsError tool result with a single text message.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
