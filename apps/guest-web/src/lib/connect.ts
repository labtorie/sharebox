// Establishes and maintains the guest's connection to a host.
//
// Order of preference:
//   1. Direct HTTPS to the host (only if the host verified that it is
//      reachable from the internet and has a trusted certificate). The page
//      then moves to the host's own origin.
//   2. WebRTC DataChannel. ICE picks a direct peer-to-peer path when one
//      exists (LAN, STUN hole punching) and falls back to the TURN relay
//      automatically. The selected candidate pair tells us which was used.

import * as P from "./protocol";
import { attachDataChannel } from "./tunnel";
import { RTCTransport, type Kind, type Transport } from "./transport";

export type ConnState =
  | { phase: "connecting"; step: string }
  | { phase: "connected"; kind: Kind }
  | { phase: "reconnecting"; reason: string }
  | { phase: "host-offline" }
  | { phase: "failed"; title: string; detail: string; retryable: boolean };

const FATAL: Record<string, [string, string]> = {
  not_found: ["This share doesn't exist", "The link may be mistyped, or the host has stopped sharing."],
  unauthorized: ["This link is no longer valid", "The host may have regenerated the link or ended your access. Ask them for a new one."],
  expired: ["This share has ended", "The link has expired or the host stopped sharing."],
};

function wsURL(): string {
  const u = new URL("/ws/guest", location.href);
  u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
  return u.toString();
}

export class Connector {
  private ws: WebSocket | null = null;
  private pc: RTCPeerConnection | null = null;
  private current: RTCTransport | null = null;
  private ice: P.ICEServer[] = [];
  private online = false;
  private stopped = false;
  private failedHard = false;
  private attempt = 0;
  private waiters: ((t: Transport) => void)[] = [];
  private negotiating = false;
  private wsBackoff = 1000;
  private pendingCandidates: RTCIceCandidateInit[] = [];
  state: ConnState = { phase: "connecting", step: "Contacting the Sharebox server" };

  constructor(
    private sessionId: string,
    private key: string,
    private onState: (s: ConnState) => void,
    private onDirect: (url: string) => void,
  ) {}

  private set(s: ConnState) {
    this.state = s;
    this.onState(s);
  }

  start() {
    this.stopped = false;
    if (!window.isSecureContext || !crypto.subtle) {
      this.set({ phase: "failed", title: "Secure connection required", detail: "Open this link over https://.", retryable: false });
      return;
    }
    if (typeof RTCPeerConnection === "undefined") {
      this.set({ phase: "failed", title: "Browser not supported", detail: "This browser does not support WebRTC. Try a current version of Chrome, Firefox, Safari or Edge.", retryable: false });
      return;
    }
    this.openWS();
  }

  stop() {
    this.stopped = true;
    this.ws?.close();
    this.current?.close();
  }

  retry() {
    this.failedHard = false;
    this.attempt = 0;
    this.set({ phase: "connecting", step: "Contacting the Sharebox server" });
    if (!this.ws || this.ws.readyState > 1) this.openWS();
    else this.negotiate();
  }

  /** Resolves with a live transport, waiting through reconnects. */
  transport(): Promise<Transport> {
    if (this.current && !this.current.closed()) return Promise.resolve(this.current);
    return new Promise((res) => this.waiters.push(res));
  }

  private async openWS() {
    if (this.stopped || this.failedHard) return;
    const ws = new WebSocket(wsURL());
    this.ws = ws;
    ws.onopen = async () => {
      this.wsBackoff = 1000;
      ws.send(JSON.stringify({ t: "join", session: this.sessionId, signal: await P.signalToken(this.key) }));
    };
    ws.onmessage = (ev) => {
      let m: P.Envelope;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.onMessage(m);
    };
    ws.onclose = () => {
      if (this.ws !== ws || this.stopped || this.failedHard) return;
      this.ws = null;
      // An established DataChannel keeps working without signalling.
      if (!this.current || this.current.closed()) {
        this.set({ phase: "reconnecting", reason: "Lost contact with the Sharebox server" });
      }
      setTimeout(() => this.openWS(), this.wsBackoff);
      this.wsBackoff = Math.min(this.wsBackoff * 2, 15000);
    };
  }

  private send(m: P.Envelope) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }

  private async onMessage(m: P.Envelope) {
    switch (m.t) {
      case "error": {
        const f = m.code ? FATAL[m.code] : undefined;
        if (f) {
          this.failedHard = true;
          this.current?.close();
          this.set({ phase: "failed", title: f[0], detail: f[1], retryable: false });
        } else {
          this.set({ phase: "failed", title: "Couldn't connect", detail: m.message || "Unknown error", retryable: true });
        }
        return;
      }
      case "joined":
        this.ice = m.iceServers ?? [];
        this.online = !!m.online;
        if (this.current && !this.current.closed()) return; // signalling came back; keep the channel
        if (!this.online) {
          this.set({ phase: "host-offline" });
          return;
        }
        this.begin(m.direct);
        return;
      case "host-offline":
        this.online = false;
        if (!this.current || this.current.closed()) this.set({ phase: "host-offline" });
        return;
      case "host-online":
        this.online = true;
        if (!this.current || this.current.closed()) this.begin(m.direct);
        return;
      case "signal":
        if (m.data) this.onSignal(m.data);
        return;
    }
  }

  private async begin(direct?: string) {
    if (direct && (await this.tryDirect(direct))) return;
    this.negotiate();
  }

  private async tryDirect(url: string): Promise<boolean> {
    this.set({ phase: "connecting", step: "Trying a direct connection" });
    try {
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 3500);
      const res = await fetch(url + "/api/ping", { signal: ctl.signal, mode: "cors", cache: "no-store" });
      clearTimeout(t);
      const j = await res.json();
      if (j?.sharebox === "host") {
        this.onDirect(url);
        return true;
      }
    } catch {
      /* fall through to WebRTC */
    }
    return false;
  }

  private async negotiate() {
    if (this.negotiating || this.stopped || this.failedHard) return;
    this.negotiating = true;
    let lastErr = "";
    try {
      for (this.attempt = 1; this.attempt <= 3; this.attempt++) {
        if (this.stopped || this.failedHard) return;
        this.set({ phase: "connecting", step: this.attempt > 1 ? "Retrying the connection to the host" : "Establishing a secure peer-to-peer connection" });
        try {
          const t = await this.connectRTC();
          this.current = t;
          this.set({ phase: "connected", kind: t.kind });
          const ws = this.waiters;
          this.waiters = [];
          ws.forEach((w) => w(t));
          return;
        } catch (e) {
          lastErr = (e as Error).message;
          if (this.failedHard) return;
          await new Promise((r) => setTimeout(r, 1000 * this.attempt));
        }
      }
      const relay = this.ice.some((s) => s.urls.some((u) => u.startsWith("turn")));
      this.set({
        phase: "failed",
        title: "Couldn't connect to the host",
        detail: relay
          ? `No network path to the host worked, including the relay (${lastErr}). Your network may block WebRTC; try another network.`
          : `A direct connection wasn't possible and this server has no relay configured (${lastErr}). Try another network, or ask the host to configure a relay.`,
        retryable: true,
      });
    } finally {
      this.negotiating = false;
    }
  }

  private connectRTC(): Promise<RTCTransport> {
    return new Promise(async (resolve, reject) => {
      this.pc?.close();
      // ?transport=relay forces the TURN relay (diagnostics and testing).
      const relayOnly = new URLSearchParams(location.search).get("transport") === "relay";
      const pc = new RTCPeerConnection({
        iceServers: this.ice.map((s) => ({ urls: s.urls, username: s.username, credential: s.credential })),
        iceTransportPolicy: relayOnly ? "relay" : "all",
      });
      this.pc = pc;
      this.pendingCandidates = [];
      const dc = pc.createDataChannel(P.TunnelLabel, { ordered: true });
      let done = false;
      const timer = setTimeout(() => fail(new Error("timed out")), 25000);
      const fail = (e: Error) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        pc.close();
        reject(e);
      };
      pc.onicecandidate = (ev) => {
        if (!ev.candidate) return;
        const c = ev.candidate.toJSON();
        this.send({ t: "signal", data: { kind: "candidate", candidate: c.candidate, sdpMid: c.sdpMid ?? "", sdpMLineIndex: c.sdpMLineIndex ?? 0 } });
      };
      pc.onconnectionstatechange = () => {
        if (pc.connectionState === "failed") fail(new Error("connection failed"));
      };
      dc.onopen = async () => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        const kind = await selectedKind(pc);
        const client = attachDataChannel(dc);
        const t = new RTCTransport(kind, client, pc);
        client.onclose = () => this.onLost(t);
        resolve(t);
      };
      try {
        const offer = await pc.createOffer();
        await pc.setLocalDescription(offer);
        this.send({ t: "signal", data: { kind: "offer", sdp: offer.sdp, policy: relayOnly ? "relay" : undefined } });
      } catch (e) {
        fail(e as Error);
      }
    });
  }

  private async onSignal(d: P.SignalPayload) {
    const pc = this.pc;
    if (!pc) return;
    if (d.kind === "answer" && d.sdp) {
      // Verify that the answer comes from a host that knows the access key,
      // so a tampering signalling server cannot insert itself (MITM).
      const want = await P.fingerprintMAC(this.key, P.sdpFingerprint(d.sdp));
      if (!d.fpmac || !P.constantTimeEqual(d.fpmac, want)) {
        this.failedHard = true;
        pc.close();
        this.set({ phase: "failed", title: "Host identity could not be verified", detail: "The connection was refused for your safety. The link may be outdated, or something is interfering with the connection.", retryable: true });
        return;
      }
      if (pc.signalingState !== "have-local-offer") return;
      await pc.setRemoteDescription({ type: "answer", sdp: d.sdp });
      for (const c of this.pendingCandidates) await pc.addIceCandidate(c).catch(() => {});
      this.pendingCandidates = [];
    } else if (d.kind === "candidate" && d.candidate) {
      const c = { candidate: d.candidate, sdpMid: d.sdpMid || "0", sdpMLineIndex: d.sdpMLineIndex ?? 0 };
      if (!pc.remoteDescription) this.pendingCandidates.push(c);
      else await pc.addIceCandidate(c).catch(() => {});
    }
  }

  private onLost(t: RTCTransport) {
    if (this.current !== t || this.stopped || this.failedHard) return;
    this.current = null;
    this.pc?.close();
    this.set({ phase: "reconnecting", reason: "The connection to the host was interrupted" });
    if (this.online && this.ws?.readyState === WebSocket.OPEN) {
      setTimeout(() => this.negotiate(), 500);
    } else if (!this.ws) {
      this.openWS();
    }
  }
}

async function selectedKind(pc: RTCPeerConnection): Promise<Kind> {
  try {
    const stats = await pc.getStats();
    let pairId = "";
    stats.forEach((s) => {
      if (s.type === "transport" && s.selectedCandidatePairId) pairId = s.selectedCandidatePairId;
    });
    let pair: RTCIceCandidatePairStats | undefined;
    stats.forEach((s) => {
      if (s.type === "candidate-pair" && (s.id === pairId || (!pairId && s.nominated && s.state === "succeeded"))) pair = s;
    });
    if (pair) {
      const local = stats.get(pair.localCandidateId);
      const remote = stats.get(pair.remoteCandidateId);
      if (local?.candidateType === "relay" || remote?.candidateType === "relay") return "relay";
    }
  } catch {
    /* ignore */
  }
  return "p2p";
}
