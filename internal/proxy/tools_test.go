package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordingMintHandler fakes the sidecar mint API and records requests.
type recordingMintHandler struct {
	mu       sync.Mutex
	hits     int
	lastAuth string
	lastBody map[string]any
	status   int // 0 → serve respBody with 200
	respBody string
}

func (h *recordingMintHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
	h.lastAuth = r.Header.Get("Authorization")
	h.lastBody = m

	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(h.respBody))
}

// snapshot returns the recorded state under lock.
func (h *recordingMintHandler) snapshot() (hits int, auth string, body map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits, h.lastAuth, h.lastBody
}

// fakeFileHandler fakes the sidecar file endpoint (signed URL target).
type fakeFileHandler struct {
	mu     sync.Mutex
	hits   int
	data   []byte
	ct     string
	status int // 0 → 200 with data
}

func (h *fakeFileHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	status, data, ct := h.status, h.data, h.ct
	h.hits++
	h.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (h *fakeFileHandler) hitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

const signedURL = "http://signed.example/media/abc123/file.bin?exp=4102444800&sig=deadbeef"

func mintBody(size int64, mime string) string {
	expires := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"url":%q,"expires_at":%q,"size_bytes":%d,"mime_type":%q}`, signedURL, expires, size, mime)
}

// mediaFixture wires fake mint + fake file server into a downstream MCP
// server carrying the generic tools, and connects a client session.
type mediaFixture struct {
	ctx     context.Context
	session *mcp.ClientSession
	mint    *recordingMintHandler
	files   *fakeFileHandler
}

func newMediaFixture(t *testing.T, mintStatus int, mintResp string, tweak func(fileURL string) MediaToolOptions) *mediaFixture {
	t.Helper()
	ctx := context.Background()

	fh := &fakeFileHandler{data: []byte("\x89PNG-fake-bytes"), ct: "image/png"}
	fileServer := httptest.NewServer(fh)
	t.Cleanup(fileServer.Close)

	mh := &recordingMintHandler{status: mintStatus, respBody: mintResp}
	mintServer := httptest.NewServer(mh)
	t.Cleanup(mintServer.Close)

	mc, err := NewMintClient(mintServer.URL, "test-token", nil)
	if err != nil {
		t.Fatalf("NewMintClient: %v", err)
	}

	opts := MediaToolOptions{}
	if tweak != nil {
		opts = tweak(fileServer.URL)
	}
	opts.Logger = testLogger()

	server := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "0.1.0"}, nil)
	RegisterMediaTools(server, mc, opts)

	httpSrv := serveMCP(t, server)
	session := connectClient(t, ctx, httpSrv.URL)
	return &mediaFixture{ctx: ctx, session: session, mint: mh, files: fh}
}

// structuredMap normalizes the result's structured content to a plain map.
func structuredMap(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("structured content is not an object: %s", raw)
	}
	return m
}

func texts(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

func TestDownloadFileHappyPath(t *testing.T) {
	fx := newMediaFixture(t, 0, mintBody(14, "text/plain"), nil)

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name:      "download_file",
		Arguments: map[string]any{"path": helloFilePath},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", texts(res))
	}

	sc := structuredMap(t, res)
	if sc["url"] != signedURL {
		t.Errorf("url = %v, want %v", sc["url"], signedURL)
	}
	if sc["mime_type"] != "text/plain" {
		t.Errorf("mime_type = %v", sc["mime_type"])
	}
	if sc["size_bytes"] != float64(14) {
		t.Errorf("size_bytes = %v, want 14", sc["size_bytes"])
	}
	if sc["filename"] != "hello-world.txt" {
		t.Errorf("filename = %v, want hello-world.txt", sc["filename"])
	}
	if _, ok := sc["inline_included"].(bool); !ok {
		t.Errorf("inline_included missing/not bool: %v", sc["inline_included"])
	}
	if b, _ := sc["inline_included"].(bool); b {
		t.Error("inline_included = true, want false")
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(sc["expires_at"])); err != nil {
		t.Errorf("expires_at not RFC3339: %v (%v)", sc["expires_at"], err)
	}
	textsGot := texts(res)
	if len(textsGot) != 1 || !strings.Contains(textsGot[0], signedURL) {
		t.Errorf("expected exactly one TextContent with the URL, got %q", textsGot)
	}

	hits, auth, body := fx.mint.snapshot()
	if hits != 1 {
		t.Fatalf("mint hits = %d, want 1", hits)
	}
	if auth != "Bearer test-token" {
		t.Errorf("mint auth header = %q", auth)
	}
	if body["path"] != helloFilePath {
		t.Errorf("mint path = %v", body["path"])
	}
	if body["disposition"] != "attachment" {
		t.Errorf("disposition = %v, want attachment", body["disposition"])
	}
	if _, ok := body["ttl_seconds"]; ok {
		t.Error("ttl_seconds sent although unset")
	}
	if fx.files.hitCount() != 0 {
		t.Error("file server consulted although mode=url")
	}
}

func TestStreamMediaImageInlineSwapsFetchBase(t *testing.T) {
	wantData := []byte("\x89PNG-real-image-payload")
	fx := newMediaFixture(t, 0, mintBody(int64(len(wantData)), "image/png"),
		func(fileURL string) MediaToolOptions { return MediaToolOptions{FetchBaseURL: fileURL} })
	fx.files.data = wantData

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name:      "stream_media",
		Arguments: map[string]any{"path": "/tmp/pic.png"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", texts(res))
	}
	if fx.files.hitCount() != 1 {
		t.Fatalf("file server hits = %d, want 1 (base swap must redirect the inline fetch)", fx.files.hitCount())
	}

	var img *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
			break
		}
	}
	if img == nil {
		t.Fatalf("no ImageContent in result, contents: %T", res.Content)
	}
	if string(img.Data) != string(wantData) {
		t.Errorf("image data round-trip mismatch")
	}
	if img.MIMEType != "image/png" {
		t.Errorf("mime type = %q", img.MIMEType)
	}
	if sc := structuredMap(t, res); sc["inline_included"] != true {
		t.Errorf("inline_included = %v, want true", sc["inline_included"])
	}

	_, _, body := fx.mint.snapshot()
	if body["disposition"] != "inline" {
		t.Errorf("disposition = %v, want inline", body["disposition"])
	}
}

func TestStreamMediaAudioInline(t *testing.T) {
	wantData := []byte("RIFF-fake-wav-data")
	fx := newMediaFixture(t, 0, mintBody(int64(len(wantData)), "audio/wav"),
		func(fileURL string) MediaToolOptions { return MediaToolOptions{FetchBaseURL: fileURL} })
	fx.files.data = wantData
	fx.files.ct = "audio/wav"

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name:      "stream_media",
		Arguments: map[string]any{"path": "/tmp/sound.wav"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", texts(res))
	}
	var aud *mcp.AudioContent
	for _, c := range res.Content {
		if ac, ok := c.(*mcp.AudioContent); ok {
			aud = ac
			break
		}
	}
	if aud == nil {
		t.Fatalf("no AudioContent in result")
	}
	if string(aud.Data) != string(wantData) || aud.MIMEType != "audio/wav" {
		t.Errorf("audio round-trip mismatch: %d bytes, mime %q", len(aud.Data), aud.MIMEType)
	}
}

func TestModesGateInlineInclusion(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		args       map[string]any
		wantInline bool
		wantHits   int
	}{
		{name: "mode=url skips inline", tool: "stream_media", args: map[string]any{"path": "/tmp/pic.png", "mode": "url"}},
		{name: "download_file default skips inline", tool: "download_file", args: map[string]any{"path": "/tmp/pic.png"}},
		{name: "mode=both includes inline", tool: "stream_media", args: map[string]any{"path": "/tmp/pic.png", "mode": "both"}, wantInline: true, wantHits: 1},
		{name: "stream_media default includes inline", tool: "stream_media", args: map[string]any{"path": "/tmp/pic.png"}, wantInline: true, wantHits: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMediaFixture(t, 0, mintBody(20, "image/png"),
				func(fileURL string) MediaToolOptions { return MediaToolOptions{FetchBaseURL: fileURL} })

			res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected tool error: %v", texts(res))
			}
			for _, c := range res.Content {
				switch c.(type) {
				case *mcp.ImageContent:
					if !tc.wantInline {
						t.Error("unexpected ImageContent")
					}
				case *mcp.AudioContent:
					t.Error("unexpected AudioContent")
				}
			}
			if sc := structuredMap(t, res); sc["inline_included"] != tc.wantInline {
				t.Errorf("inline_included = %v, want %v", sc["inline_included"], tc.wantInline)
			}
			if got := fx.files.hitCount(); got != tc.wantHits {
				t.Errorf("file server hits = %d, want %d", got, tc.wantHits)
			}
		})
	}
}

func TestInlineSkipsForSizeAndMime(t *testing.T) {
	t.Run("oversize reports note without fetching", func(t *testing.T) {
		fx := newMediaFixture(t, 0, mintBody(200000, "image/png"),
			func(fileURL string) MediaToolOptions { return MediaToolOptions{FetchBaseURL: fileURL} })
		res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
			Name: "stream_media", Arguments: map[string]any{"path": "/tmp/big.png"},
		})
		if err != nil || res.IsError {
			t.Fatalf("call must succeed, got err=%v isError=%v %v", err, res.IsError, texts(res))
		}
		joined := strings.Join(texts(res), "\n")
		if !strings.Contains(joined, "too large") {
			t.Errorf("missing oversize note: %q", joined)
		}
		if sc := structuredMap(t, res); sc["inline_included"] != false {
			t.Errorf("inline_included = %v", sc["inline_included"])
		}
		if fx.files.hitCount() != 0 {
			t.Error("file fetched despite oversize short-circuit")
		}
	})

	t.Run("non-media mime is skipped", func(t *testing.T) {
		fx := newMediaFixture(t, 0, mintBody(5, "text/plain"), nil)
		res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
			Name: "stream_media", Arguments: map[string]any{"path": "/tmp/doc.txt"},
		})
		if err != nil || res.IsError {
			t.Fatalf("call must succeed, got err=%v isError=%v", err, res.IsError)
		}
		if !strings.Contains(strings.Join(texts(res), "\n"), "not inline-eligible") {
			t.Errorf("missing mime note: %q", texts(res))
		}
	})
}

func TestInlineFetchFailureStillSucceeds(t *testing.T) {
	fx := newMediaFixture(t, 0, mintBody(20, "image/png"),
		func(fileURL string) MediaToolOptions { return MediaToolOptions{FetchBaseURL: fileURL} })
	fx.files.status = http.StatusInternalServerError

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name: "stream_media", Arguments: map[string]any{"path": "/tmp/pic.png"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("fetch failure must downgrade to note, got error result: %v", texts(res))
	}
	if sc := structuredMap(t, res); sc["inline_included"] != false {
		t.Errorf("inline_included = %v, want false", sc["inline_included"])
	}
	if !strings.Contains(strings.Join(texts(res), "\n"), "inline fetch failed") {
		t.Errorf("missing failure note: %q", texts(res))
	}
}

func TestMintErrorBecomesIsErrorWithoutTokenLeak(t *testing.T) {
	fx := newMediaFixture(t, http.StatusForbidden, `{"error":"forbidden by policy"}`, nil)

	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name: "download_file", Arguments: map[string]any{"path": helloFilePath},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result on mint failure")
	}
	joined := strings.Join(texts(res), "\n")
	if !strings.Contains(joined, "403") || !strings.Contains(joined, "forbidden by policy") {
		t.Errorf("error message should carry status + mint error, got %q", joined)
	}
	if strings.Contains(joined, "test-token") {
		t.Error("token leaked into tool result")
	}
	if res.StructuredContent != nil {
		t.Errorf("no structured content expected on error, got %v", res.StructuredContent)
	}
}

func TestInvalidModeIsRejectedBeforeMint(t *testing.T) {
	fx := newMediaFixture(t, 0, mintBody(5, "text/plain"), nil)
	res, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name: "stream_media", Arguments: map[string]any{"path": "/tmp/x", "mode": "sideways"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for invalid mode")
	}
	if !strings.Contains(strings.Join(texts(res), "\n"), "invalid mode") {
		t.Errorf("unexpected message: %q", texts(res))
	}
	if hits, _, _ := fx.mint.snapshot(); hits != 0 {
		t.Errorf("mint called %d times despite invalid mode", hits)
	}
}

func TestTTLSecondsPassedThrough(t *testing.T) {
	fx := newMediaFixture(t, 0, mintBody(5, "text/plain"), nil)
	if _, err := fx.session.CallTool(fx.ctx, &mcp.CallToolParams{
		Name:      "stream_media",
		Arguments: map[string]any{"path": "/tmp/x", "ttl_seconds": 120},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	_, _, body := fx.mint.snapshot()
	if body["ttl_seconds"] != float64(120) {
		t.Errorf("ttl_seconds = %v, want 120", body["ttl_seconds"])
	}
}

func TestSwapBase(t *testing.T) {
	const raw = "http://signed.example/media/a/f.png?exp=1&sig=x"
	got, err := swapBase(raw, "https://fetch.internal:8090")
	if err != nil {
		t.Fatalf("swapBase: %v", err)
	}
	want := "https://fetch.internal:8090/media/a/f.png?exp=1&sig=x"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
	same, err := swapBase(raw, "")
	if err != nil || same != raw {
		t.Errorf("empty base must keep URL, got %q err=%v", same, err)
	}
	if _, err := swapBase(raw, "%zz"); err == nil {
		t.Error("invalid base must error")
	}
}

func TestNewMintClientValidation(t *testing.T) {
	if _, err := NewMintClient("not-a-url", "tok", nil); err == nil {
		t.Error("host-less URL must be rejected")
	}
	if _, err := NewMintClient("ftp://example.com", "tok", nil); err == nil {
		t.Error("non-http scheme must be rejected")
	}
	if _, err := NewMintClient("http://localhost:8091", "", nil); err == nil {
		t.Error("empty token must be rejected")
	}
	if _, err := NewMintClient("http://localhost:8091", "tok", nil); err != nil {
		t.Errorf("valid inputs rejected: %v", err)
	}
}
