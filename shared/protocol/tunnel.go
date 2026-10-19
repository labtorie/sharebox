// Package protocol defines wire formats shared by the host, the coordination
// server and the guest web app. apps/guest-web/src/lib/protocol.ts mirrors
// these definitions and must be kept in sync.
package protocol

// DataChannel tunnel ("sbx/1")
//
// The guest opens one ordered, reliable RTCDataChannel labelled
// TunnelLabel. HTTP requests are multiplexed over it as streams so the host
// can feed them to the very same http.Handler used for direct HTTPS.
//
// Every DataChannel message is one frame:
//
//	offset size  field
//	0      1     type
//	1      4     stream id (big endian; odd = guest initiated; 0 = control)
//	5      4     sequence number of DATA frames, per stream and direction
//	9      n     payload
//
// Frame types:
//
//	HEADERS  JSON. Request: {"m":method,"u":uri,"h":headers,"b":hasBody}
//	               Response: {"s":status,"h":headers}
//	DATA     raw body bytes (never base64). seq starts at 0 and increments by
//	         one; a gap or reordering resets the stream (corruption guard).
//	END      8-byte big-endian total body length. The receiver compares it
//	         with the bytes it actually received.
//	WINDOW   4-byte big-endian credit increment. Acknowledges consumed bytes
//	         and allows the peer to send that much more (flow control).
//	RESET    JSON {"c":code,"m":message}. Aborts the stream (cancellation or
//	         error). Either side may send it.
//	PING     8 opaque bytes, answered with PONG carrying the same bytes.
//
// Flow control: each stream direction starts with InitialWindow bytes of
// credit. A sender never has more than its credit in flight; receivers
// return credit as the application consumes data. Independently, senders
// pause while the DataChannel's bufferedAmount exceeds HighWaterMark.
//
// Recovery: if the channel drops, all streams fail. Downloads resume with
// HTTP Range requests and uploads resume from the offset reported by
// GET /api/uploads/{id}, over a newly negotiated connection.
const (
	TunnelLabel = "sbx/1"

	FrameHeaders byte = 0x01
	FrameData    byte = 0x02
	FrameEnd     byte = 0x03
	FrameWindow  byte = 0x04
	FrameReset   byte = 0x05
	FramePing    byte = 0x06
	FramePong    byte = 0x07

	HeaderSize      = 9
	MaxMessageSize  = 16 * 1024 // interoperable SCTP message size for all browsers
	MaxPayload      = MaxMessageSize - HeaderSize
	InitialWindow   = 1 << 20
	HighWaterMark   = 1 << 20
	LowWaterMark    = 256 << 10
	MaxStreams      = 32
	PingIntervalSec = 10
	DeadAfterSec    = 35
)

// RequestHead is the payload of a request HEADERS frame.
type RequestHead struct {
	Method  string              `json:"m"`
	URI     string              `json:"u"`
	Header  map[string][]string `json:"h"`
	HasBody bool                `json:"b"`
}

// ResponseHead is the payload of a response HEADERS frame.
type ResponseHead struct {
	Status int                 `json:"s"`
	Header map[string][]string `json:"h"`
}

// Reset is the payload of a RESET frame.
type Reset struct {
	Code    string `json:"c"`
	Message string `json:"m,omitempty"`
}

// Reset codes.
const (
	ResetCancel   = "cancel"
	ResetProtocol = "protocol"
	ResetBusy     = "busy"
	ResetInternal = "internal"
)
