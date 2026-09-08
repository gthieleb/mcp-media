package proxy

// Mode selects which proxy mechanisms are active.
type Mode string

// ModeFull mirrors an upstream MCP server (tools + enrichment). It is the
// default for MCP-server pods.
const ModeFull Mode = "full"

// ModeStandalone serves only the generic stream_media/download_file tools
// with no upstream connection and no enrichment. It is the egress surface
// for agent pods (Wave 6).
const ModeStandalone Mode = "standalone"

// Wiring is the per-mode component matrix: which proxy mechanisms run() must
// activate for a Mode.
type Wiring struct {
	// MirrorUpstream connects the persistent upstream session and mirrors
	// its tools onto the downstream server.
	MirrorUpstream bool
	// Enrich installs the TOOL_MATCH enrichment middleware.
	Enrich bool
}

// WiringForMode returns the component matrix for mode.
func WiringForMode(mode Mode) Wiring {
	switch mode {
	case ModeFull:
		return Wiring{MirrorUpstream: true, Enrich: true}
	case ModeStandalone:
		return Wiring{}
	default:
		// Unreachable: Mode values are constructed only by this package and
		// config parsing validates against the two known values.
		return Wiring{MirrorUpstream: true, Enrich: true}
	}
}
