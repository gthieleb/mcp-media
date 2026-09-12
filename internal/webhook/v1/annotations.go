package v1

import (
	"fmt"
	"strconv"
)

// Annotation keys understood by the media webhook. All annotations live on
// the Pod template metadata (i.e. the Pod's own metadata when the webhook
// sees it).
const (
	// InjectSidecar injects the media-sidecar file server (readOnly volume).
	InjectSidecar = "media.media/inject-sidecar"
	// InjectProxy injects the mcp-media-proxy MCP proxy.
	InjectProxy = "media.media/inject-proxy"
	// VolumeName names the PVC the sidecar mounts readOnly. The literal
	// value "empty" (or omission) selects an emptyDir volume.
	VolumeName = "media.media/volume-name"
	// VolumePath is the mount path of the media volume in the sidecar
	// (default /data).
	VolumePath = "media.media/volume-path"
	// UpstreamPort is the main container's MCP port the proxy forwards to.
	UpstreamPort = "media.media/upstream-port"
	// ProxyMode selects the proxy mode ("full" default, "standalone").
	ProxyMode = "media.media/proxy-mode"
	// ToolMatch is the TOOL_MATCH regex for the enrichment middleware.
	ToolMatch = "media.media/tool-match"
	// MintURL overrides the proxy MINT_URL (default: localhost:8091 when
	// the sidecar lives in the same pod).
	MintURL = "media.media/mint-url"
	// PublicURL sets MEDIA_PUBLIC_BASE_URL on the sidecar.
	PublicURL = "media.media/public-url"
	// InlineMaxBytes overrides the proxy INLINE_MAX_BYTES.
	InlineMaxBytes = "media.media/inline-max-bytes"
	// Group links sidecar and proxy across deployments (split topology):
	// the proxy mints via "<group>-sidecar-service:8091".
	Group = "media.media/group"

	// InjectedLabel marks pods whose template was mutated (the Reconciler
	// watch signal per plan T3.2).
	InjectedLabel = "media.media/injected"

	// mediaVolumeName is the name of the injected volume.
	mediaVolumeName = "media-volume"

	defaultVolumePath = "/data"
	sidecarName       = "media-sidecar"
	proxyName         = "mcp-media-proxy"

	sidecarServePort = 8090
	sidecarMintPort  = 8091
	proxyListenPort  = 5780
)

// injectionRequest is the parsed, validated view of a pod's media
// annotations. Parse is the single boundary: downstream code reads typed
// fields only.
type injectionRequest struct {
	InjectSidecar  bool
	InjectProxy    bool
	VolumeName     string // PVC claim name; "" → emptyDir
	VolumePath     string
	UpstreamPort   string // "" → not set
	ProxyMode      string // "" → "full" (proxy default)
	ToolMatch      string // "" → enrichment disabled
	MintURL        string // explicit override; "" → derived
	PublicURL      string // sidecar MEDIA_PUBLIC_BASE_URL
	InlineMaxBytes string // raw; validated by the proxy itself
	Group          string // split topology; "" → single-pod
}

// sidecarImage / proxyImage / secret names are injected at webhook startup
// (controller configuration, not pod annotations).
var (
	sidecarImage         = "ghcr.io/gthieleb/mcp-media-sidecar:latest"
	proxyImage           = "ghcr.io/gthieleb/mcp-media-proxy:latest"
	signingSecretName    = "media-signing"
	internalTokenSecret  = "media-internal-token"
	defaultPublicBaseURL = ""
)

// ConfigureImages sets the container images and secret names used by the
// webhook. Called once at manager startup (T3.4 values plumbing).
func ConfigureImages(sidecar, proxy, signingSecret, tokenSecret string) {
	if sidecar != "" {
		sidecarImage = sidecar
	}
	if proxy != "" {
		proxyImage = proxy
	}
	if signingSecret != "" {
		signingSecretName = signingSecret
	}
	if tokenSecret != "" {
		internalTokenSecret = tokenSecret
	}
}

// parseAnnotations extracts the injection request from pod annotations.
// Unknown or malformed values that cannot affect injection are ignored
// (fail-open per failurePolicy: Ignore); booleans must be exactly "true".
func parseAnnotations(annotations map[string]string) (injectionRequest, error) {
	req := injectionRequest{}
	if annotations == nil {
		return req, nil
	}
	req.InjectSidecar = annotations[InjectSidecar] == "true"
	req.InjectProxy = annotations[InjectProxy] == "true"
	req.VolumeName = annotations[VolumeName]
	req.VolumePath = annotations[VolumePath]
	req.UpstreamPort = annotations[UpstreamPort]
	req.ProxyMode = annotations[ProxyMode]
	req.ToolMatch = annotations[ToolMatch]
	req.MintURL = annotations[MintURL]
	req.PublicURL = annotations[PublicURL]
	req.InlineMaxBytes = annotations[InlineMaxBytes]
	req.Group = annotations[Group]

	if req.InjectSidecar && req.VolumeName != "" && req.VolumeName != "empty" &&
		!isRFC1123Label(req.VolumeName) {
		return req, fmt.Errorf("%s %q is not a valid PVC name", VolumeName, req.VolumeName)
	}
	if req.InjectProxy && req.UpstreamPort != "" {
		if _, err := strconv.Atoi(req.UpstreamPort); err != nil {
			return req, fmt.Errorf("%s %q is not a port number", UpstreamPort, req.UpstreamPort)
		}
	}
	return req, nil
}

// isRFC1123Label checks the lowercase-alnum-with-dashes subset that
// Kubernetes names use (good enough to reject obviously broken claims).
func isRFC1123Label(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
