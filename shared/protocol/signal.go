package protocol

// Signalling protocol between host, coordination server and guest. Every
// message is a JSON object sent as one WebSocket text message.
//
// Host:   wss://<server>/ws/host
// Guest:  wss://<server>/ws/guest
//
// The coordination server never sees the access key from the link fragment.
// Guests prove they hold the link by presenting SignalToken = HMAC(key,
// "sharebox-signal-v1"); the host registers only SHA-256(SignalToken). File
// access always requires the key itself, verified by the host over the
// end-to-end encrypted channel.

// Message types.
const (
	// host -> server
	MsgRegister   = "register"
	MsgUnregister = "unregister"
	MsgSetDirect  = "set-direct"
	MsgProbe      = "probe"
	MsgCert       = "cert"
	MsgKick       = "kick"
	MsgTurnCreds  = "turn-creds"

	// server -> host
	MsgRegistered  = "registered"
	MsgGuestJoined = "guest-joined"
	MsgGuestLeft   = "guest-left"
	MsgResult      = "result" // reply to a request carrying an id

	// guest -> server
	MsgJoin = "join"

	// server -> guest
	MsgJoined      = "joined"
	MsgHostOffline = "host-offline"
	MsgHostOnline  = "host-online"

	// both directions
	MsgSignal = "signal"
	MsgPing   = "ping"
	MsgPong   = "pong"
	MsgError  = "error"
)

// Error codes sent to guests.
const (
	ErrCodeNotFound     = "not_found"
	ErrCodeUnauthorized = "unauthorized"
	ErrCodeExpired      = "expired"
	ErrCodeRateLimited  = "rate_limited"
	ErrCodeBadRequest   = "bad_request"
)

// ICEServer mirrors RTCIceServer.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Envelope is the union of all message fields.
type Envelope struct {
	Type string `json:"t"`
	ID   string `json:"id,omitempty"` // request id for request/response pairs

	// register
	SignalHash string `json:"signalHash,omitempty"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"` // unix seconds; 0 = no expiry
	SessionID  string `json:"session,omitempty"`
	HostToken  string `json:"hostToken,omitempty"` // to resume a session after reconnect
	DeviceID   string `json:"deviceId,omitempty"`
	DeviceKey  string `json:"deviceKey,omitempty"`

	// registered
	URL        string      `json:"url,omitempty"`
	ICEServers []ICEServer `json:"iceServers,omitempty"`
	ObservedIP string      `json:"observedIp,omitempty"`
	Features   *Features   `json:"features,omitempty"`
	Hostname   string      `json:"hostname,omitempty"`

	// guest join / signal routing
	Signal string         `json:"signal,omitempty"`
	Peer   string         `json:"peer,omitempty"`
	Data   *SignalPayload `json:"data,omitempty"`
	Label  string         `json:"label,omitempty"`

	// host info for guests
	Online bool   `json:"online,omitempty"`
	Direct string `json:"direct,omitempty"`

	// probe / cert
	Port  int    `json:"port,omitempty"`
	Nonce string `json:"nonce,omitempty"`
	CSR   string `json:"csr,omitempty"` // base64 DER
	Cert  string `json:"cert,omitempty"` // PEM chain
	OK    bool   `json:"ok,omitempty"`
	Body  string `json:"body,omitempty"`

	// errors
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Features advertised by the coordination server.
type Features struct {
	TURN   bool `json:"turn"`
	Probe  bool `json:"probe"`
	Direct bool `json:"direct"` // DNS + certificate issuance for direct HTTPS
}

// SignalPayload carries WebRTC negotiation data.
type SignalPayload struct {
	Kind      string `json:"kind"` // offer | answer | candidate | bye
	SDP       string `json:"sdp,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	SDPMid    string `json:"sdpMid,omitempty"`
	SDPMLine  *int   `json:"sdpMLineIndex,omitempty"`
	// FingerprintMAC authenticates the host's DTLS fingerprint with the
	// access key (see security.FingerprintMAC) so guests can detect a
	// tampering signalling server.
	FingerprintMAC string `json:"fpmac,omitempty"`
	// Policy lets a guest request relay-only ICE (used for diagnostics).
	Policy string `json:"policy,omitempty"`
	// Conn numbers a guest's parallel connections (0 = primary). Each one is
	// a separate PeerConnection with its own congestion control.
	Conn int `json:"conn,omitempty"`
	// Parallel, in the host's answer to the primary connection, is how many
	// connections the guest may open in total. Absent from older hosts, in
	// which case guests use a single connection.
	Parallel int `json:"parallel,omitempty"`
}

// MaxParallelConns is how many connections a host accepts per guest.
const MaxParallelConns = 4

// ProbePath is served by the host for the reachability check.
const ProbePath = "/.well-known/sharebox-probe"
