// Mirrors shared/protocol/tunnel.go and signal.go. Keep in sync.

export const TunnelLabel = "sbx/1";

export const FrameHeaders = 0x01;
export const FrameData = 0x02;
export const FrameEnd = 0x03;
export const FrameWindow = 0x04;
export const FrameReset = 0x05;
export const FramePing = 0x06;
export const FramePong = 0x07;

export const HeaderSize = 9;
export const MaxMessageSize = 16 * 1024;
export const MaxPayload = MaxMessageSize - HeaderSize;
export const InitialWindow = 1 << 20;
export const HighWaterMark = 1 << 20;
export const LowWaterMark = 256 << 10;
export const MaxStreams = 32;
export const PingIntervalMs = 10_000;
export const DeadAfterMs = 35_000;

export interface ICEServer {
  urls: string[];
  username?: string;
  credential?: string;
}

export interface SignalPayload {
  kind: "offer" | "answer" | "candidate" | "bye";
  sdp?: string;
  candidate?: string;
  sdpMid?: string;
  sdpMLineIndex?: number;
  fpmac?: string;
  policy?: string;
}

export interface Envelope {
  t: string;
  id?: string;
  session?: string;
  signal?: string;
  peer?: string;
  data?: SignalPayload;
  iceServers?: ICEServer[];
  online?: boolean;
  direct?: string;
  code?: string;
  message?: string;
}

// --- crypto helpers mirroring internal/security --------------------------

const enc = new TextEncoder();

function b64url(buf: ArrayBuffer): string {
  let s = "";
  const b = new Uint8Array(buf);
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** HMAC-SHA256(key, parts...) with 4-byte big-endian length prefixes. */
export async function hmac(key: string, ...parts: string[]): Promise<string> {
  const chunks: Uint8Array[] = [];
  for (const p of parts) {
    const b = enc.encode(p);
    const l = new Uint8Array(4);
    new DataView(l.buffer).setUint32(0, b.length);
    chunks.push(l, b);
  }
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const msg = new Uint8Array(total);
  let o = 0;
  for (const c of chunks) {
    msg.set(c, o);
    o += c.length;
  }
  const k = await crypto.subtle.importKey("raw", enc.encode(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return b64url(await crypto.subtle.sign("HMAC", k, msg));
}

export const signalToken = (key: string) => hmac(key, "sharebox-signal-v1");
export const fingerprintMAC = (key: string, fp: string) => hmac(key, "sharebox-dtls-v1", fp.toLowerCase());

export function sdpFingerprint(sdp: string): string {
  const m = /^a=fingerprint:sha-256 ([0-9A-Fa-f:]+)/m.exec(sdp);
  return m ? m[1] : "";
}

export function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let r = 0;
  for (let i = 0; i < a.length; i++) r |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return r === 0;
}

export async function sha256Hex(data: ArrayBuffer): Promise<string> {
  const h = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
  return Array.from(h, (x) => x.toString(16).padStart(2, "0")).join("");
}
