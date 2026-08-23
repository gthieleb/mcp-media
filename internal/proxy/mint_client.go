package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MintClient calls the media-sidecar mint API (POST /mint) to obtain
// short-lived HMAC-signed download URLs. It never logs the bearer token or
// minted (signed) URLs.
type MintClient struct {
	mintURL string
	token   string
	hc      *http.Client
}

// MintResponse mirrors the success response of POST /mint
// (see internal/mint package doc for the canonical contract).
type MintResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	SizeBytes int64     `json:"size_bytes"`
	MimeType  string    `json:"mime_type"`
}

// NewMintClient validates its inputs and returns a client for the mint API
// reachable at mintURL (e.g. "http://localhost:8091").
func NewMintClient(mintURL, token string, hc *http.Client) (*MintClient, error) {
	u, err := url.Parse(mintURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("proxy: invalid mint URL (need http/https with host)")
	}
	mintURL = strings.TrimRight(mintURL, "/")
	if token == "" {
		return nil, fmt.Errorf("proxy: mint token must not be empty")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &MintClient{mintURL: mintURL, token: token, hc: hc}, nil
}

// mintRequest is the POST /mint request body (see internal/mint).
type mintRequest struct {
	Path        string `json:"path"`
	Disposition string `json:"disposition,omitempty"`
	TTLSeconds  int64  `json:"ttl_seconds,omitempty"`
}

// Mint requests a signed URL for path. disposition is "inline" or
// "attachment"; ttlSeconds 0 means "sidecar default". Errors carry the HTTP
// status and the mint server's error message, never the token.
func (c *MintClient) Mint(ctx context.Context, path, disposition string, ttlSeconds int64) (*MintResponse, error) {
	body, err := json.Marshal(mintRequest{Path: path, Disposition: disposition, TTLSeconds: ttlSeconds})
	if err != nil {
		return nil, fmt.Errorf("proxy: encode mint request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mintURL+"/mint", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("proxy: build mint request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy: mint request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errBody)
		if errBody.Error == "" {
			errBody.Error = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("proxy: mint failed: %s: %s", resp.Status, errBody.Error)
	}

	var out MintResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&out); err != nil {
		return nil, fmt.Errorf("proxy: decode mint response: %w", err)
	}
	if out.URL == "" {
		return nil, fmt.Errorf("proxy: mint response without url")
	}
	return &out, nil
}
