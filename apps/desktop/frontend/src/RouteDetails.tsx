// Per-guest connection details: which network path the data takes, in plain
// language, plus the technical endpoints and live metrics.

import { useEffect, useRef, useState } from "react";
import { ArrowLeftRight, ChevronDown, ChevronRight, Globe, Home, Laptop, Radio, Router, Smartphone, X, Zap } from "lucide-react";
import * as Go from "../wailsjs/go/main/App";
import type { Endpoint, Guest, PeerInfo, Status } from "./types";
import { cx } from "./ui";
import { formatBytes, formatSpeed } from "./format";

/** The guest's connections: the primary one first, then parallel ones. */
export function peerFor(status: Status, guestId: string): PeerInfo[] {
  const all = status.peers ?? [];
  const main = all.find((p) => (p.guests ?? []).includes(guestId));
  if (!main) return [];
  return all.filter((p) => p.group === main.group && p.connected).sort((a, b) => a.conn - b.conn);
}

/** Sums byte counters over a guest's parallel connections. */
function totals(peers: PeerInfo[]) {
  let sent = 0;
  let recv = 0;
  for (const p of peers) {
    sent += p.path?.bytesSent ?? 0;
    recv += p.path?.bytesReceived ?? 0;
  }
  return { sent, recv };
}

type Tone = "green" | "blue" | "amber";

interface RouteText {
  title: string;
  explain: string;
  tone: Tone;
  icon: React.ReactNode;
}

function describe(g: Guest, peer?: PeerInfo): RouteText {
  if (g.transport === "direct") {
    return {
      title: "Directly over HTTPS",
      explain: "The guest's browser connects straight to this computer through the port your router opened. No server sits in between.",
      tone: "green",
      icon: <Zap className="h-4 w-4" />,
    };
  }
  if (g.transport === "local") {
    return { title: "Preview on this computer", explain: "This is your own preview window.", tone: "green", icon: <Laptop className="h-4 w-4" /> };
  }
  switch (peer?.path?.route ?? g.route) {
    case "local network":
      return {
        title: "Directly over your local network",
        explain: "Both devices are on the same network and talk to each other directly. The data never leaves your network, so this is the fastest path.",
        tone: "green",
        icon: <Home className="h-4 w-4" />,
      };
    case "router loopback":
      return {
        title: "Through your router's public address",
        explain:
          "Both devices are behind the same router, but the data goes out to the router's public side and comes back in. Many home routers are slow at this. Make sure both devices are on the same Wi-Fi network and that no VPN is running on either of them.",
        tone: "amber",
        icon: <Router className="h-4 w-4" />,
      };
    case "relay":
      return {
        title: "Through the relay server",
        explain:
          "A direct connection wasn't possible (usually because of a strict router, mobile network or firewall), so the Sharebox relay forwards the data. It stays end-to-end encrypted, but speed depends on the relay.",
        tone: "amber",
        icon: <Radio className="h-4 w-4" />,
      };
    case "internet":
      return {
        title: "Directly over the internet",
        explain:
          "The two devices are on different networks and send data straight to each other (peer-to-peer). No Sharebox server is involved; speed is limited by both internet connections.",
        tone: "blue",
        icon: <Globe className="h-4 w-4" />,
      };
    default:
      return { title: "Connecting…", explain: "The connection path hasn't been chosen yet.", tone: "blue", icon: <ArrowLeftRight className="h-4 w-4" /> };
  }
}

const typeLabel: Record<string, string> = {
  host: "Local address",
  srflx: "Public address (found via STUN)",
  prflx: "Address as seen by the other side",
  relay: "Relay server address",
};

const typeHelp: Record<string, string> = {
  host: "The device's own address on its network.",
  srflx: "The address the router shows to the internet.",
  prflx: "Discovered while connecting; usually the device's real address.",
  relay: "A port on the relay server that forwards the data.",
};

function fmtEndpoint(e: Endpoint) {
  const host = e.address.includes(":") ? `[${e.address}]` : e.address;
  return `${host}:${e.port}`;
}

function fmtDuration(since?: string) {
  if (!since) return "—";
  const s = Math.max(0, Math.round((Date.now() - Date.parse(since)) / 1000));
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`;
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
}

/** Live send/receive rates from successive byte counters. */
function useRates(sent: number, recv: number) {
  const prev = useRef<{ sent: number; recv: number; t: number } | null>(null);
  const [rates, setRates] = useState({ out: 0, in: 0 });
  useEffect(() => {
    const now = Date.now();
    const p = prev.current;
    if (p && now > p.t && sent >= p.sent && recv >= p.recv) {
      const dt = (now - p.t) / 1000;
      if (dt >= 0.5) {
        setRates({ out: (sent - p.sent) / dt, in: (recv - p.recv) / dt });
        prev.current = { sent, recv, t: now };
      }
    } else {
      prev.current = { sent, recv, t: now };
    }
  }, [sent, recv]);
  return rates;
}

const toneCls: Record<Tone, string> = {
  green: "bg-emerald-50 text-emerald-800 ring-emerald-200 dark:bg-emerald-950/50 dark:text-emerald-200 dark:ring-emerald-900",
  blue: "bg-brand-50 text-brand-800 ring-brand-100 dark:bg-brand-500/10 dark:text-brand-200 dark:ring-brand-500/20",
  amber: "bg-amber-50 text-amber-900 ring-amber-200 dark:bg-amber-950/50 dark:text-amber-200 dark:ring-amber-900",
};

const badge: Record<string, [string, string]> = {
  direct: ["Direct", "bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300"],
  p2p: ["P2P", "bg-brand-50 text-brand-700 dark:bg-brand-500/15 dark:text-brand-300"],
  relay: ["Relay", "bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-300"],
  local: ["Preview", "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300"],
};

const routeShort: Record<string, string> = {
  "local network": "local network",
  "router loopback": "via router",
  internet: "internet",
  relay: "relay",
};

export function GuestRow({ g, peers }: { g: Guest; peers: PeerInfo[] }) {
  const [open, setOpen] = useState(false);
  const peer = peers[0];
  const [label, cls] = badge[g.transport] ?? [g.transport, "bg-zinc-100"];
  const route = peer?.path?.route ?? g.route ?? "";
  return (
    <li className="py-1">
      <div className="flex items-center gap-2 text-sm">
        <button
          onClick={() => setOpen(!open)}
          className="flex min-w-0 flex-1 items-center gap-2 rounded-lg py-1 text-left hover:bg-zinc-50 dark:hover:bg-zinc-800/60"
          aria-expanded={open}
          title="Show connection details"
        >
          {open ? <ChevronDown className="h-4 w-4 shrink-0 text-zinc-400" /> : <ChevronRight className="h-4 w-4 shrink-0 text-zinc-400" />}
          <span className={cx("shrink-0 rounded-md px-1.5 py-0.5 text-[11px] font-semibold", cls)}>{label}</span>
          <span className="min-w-0 flex-1 truncate">
            {g.label || "Guest"}
            {route && g.transport !== "direct" && (
              <span className={cx("ml-1.5 text-xs", route === "router loopback" ? "text-amber-600 dark:text-amber-400" : "text-zinc-500")}>
                · {routeShort[route] ?? route}
              </span>
            )}
            {peers.length > 1 && <span className="ml-1.5 text-xs text-zinc-500">· ×{peers.length}</span>}
          </span>
        </button>
        <span className="shrink-0 text-xs text-zinc-500">
          since {new Date(g.since).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
        </span>
        <button
          onClick={() => Go.RevokeGuest(g.id)}
          className="rounded-lg p-1 text-zinc-400 hover:bg-zinc-100 hover:text-red-600 dark:hover:bg-zinc-800"
          title="Disconnect this guest"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      {open && <RouteDetails g={g} peers={peers} />}
    </li>
  );
}

function RouteDetails({ g, peers }: { g: Guest; peers: PeerInfo[] }) {
  const peer = peers[0];
  const d = describe(g, peer);
  const path = peer?.path;
  const sum = totals(peers);
  const rates = useRates(sum.sent, sum.recv);
  // Parallel connections may take different paths (rare); flag it.
  const routes = new Set(peers.map((p) => p.path?.route).filter(Boolean));
  return (
    <div className="sb-fade-in mt-1 mb-2 ml-6 space-y-3 rounded-xl bg-zinc-50 p-3 dark:bg-zinc-800/40">
      <div className={cx("flex gap-2.5 rounded-lg p-2.5 text-xs ring-1", toneCls[d.tone])}>
        <span className="mt-0.5 shrink-0">{d.icon}</span>
        <div>
          <div className="text-[13px] font-semibold">{d.title}</div>
          <div className="mt-0.5 opacity-90">{d.explain}</div>
        </div>
      </div>

      {path ? (
        <>
          <div className="grid grid-cols-[1fr_auto_1fr] items-stretch gap-2">
            <EndpointCard title="This computer" icon={<Laptop className="h-3.5 w-3.5" />} e={path.local} />
            <div className="flex flex-col items-center justify-center px-1 text-center text-[11px] text-zinc-500">
              <ArrowLeftRight className="h-4 w-4 text-zinc-400" />
              <span className="mt-1 font-medium uppercase">{path.local.protocol || "udp"}</span>
              {path.rttMs > 0 && <span>{path.rttMs < 10 ? path.rttMs.toFixed(1) : Math.round(path.rttMs)} ms</span>}
            </div>
            <EndpointCard title={g.label || "Guest"} icon={<Smartphone className="h-3.5 w-3.5" />} e={path.remote} />
          </div>
          <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs sm:grid-cols-3">
            <Metric label="Round-trip time" value={path.rttMs > 0 ? `${path.rttMs < 10 ? path.rttMs.toFixed(1) : Math.round(path.rttMs)} ms` : "—"} />
            <Metric label="Sending now" value={rates.out >= 2048 ? formatSpeed(rates.out) : "idle"} />
            <Metric label="Receiving now" value={rates.in >= 2048 ? formatSpeed(rates.in) : "idle"} />
            <Metric label={peers.length > 1 ? "Sent (all connections)" : "Sent on this connection"} value={formatBytes(sum.sent)} />
            <Metric label={peers.length > 1 ? "Received (all connections)" : "Received on this connection"} value={formatBytes(sum.recv)} />
            <Metric
              label="Parallel connections"
              value={peers.length > 1 ? `${peers.length}${routes.size > 1 ? " (mixed paths)" : ""}` : "1"}
              hint="Large files are split into pieces that travel over several connections at once. Each connection recovers from packet loss on its own, so together they are much faster on Wi-Fi and long distances."
            />
            <Metric label="Connected for" value={fmtDuration(path.since)} />
            <Metric
              label="Congestion window"
              value={path.cwndBytes ? formatBytes(path.cwndBytes) : "—"}
              hint="How much data may be in flight. It shrinks after packet loss; a small value on a fast network means losses are limiting the speed."
            />
            <Metric label="Encryption" value="DTLS, end-to-end" />
          </div>
        </>
      ) : g.transport === "direct" && g.address ? (
        <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs">
          <Metric label="Guest address" value={g.address} />
          <Metric label="Protocol" value="HTTPS (TLS)" />
        </div>
      ) : (
        <div className="text-xs text-zinc-500">No live connection details yet.</div>
      )}
    </div>
  );
}

function EndpointCard({ title, icon, e }: { title: string; icon: React.ReactNode; e: Endpoint }) {
  return (
    <div className="min-w-0 rounded-lg bg-white p-2.5 ring-1 ring-zinc-200 dark:bg-zinc-900 dark:ring-zinc-700/60">
      <div className="flex items-center gap-1.5 text-xs font-medium">
        {icon}
        <span className="truncate">{title}</span>
      </div>
      <div className="selectable mt-1 truncate font-mono text-[12px]" title={fmtEndpoint(e)}>
        {e.address}
      </div>
      <div className="font-mono text-[11px] text-zinc-500">port {e.port}</div>
      <div className="mt-0.5 text-[11px] text-zinc-500" title={typeHelp[e.type]}>
        {typeLabel[e.type] ?? e.type}
      </div>
      {e.related && (
        <div className="selectable truncate text-[11px] text-zinc-500" title="The device's own address behind this one">
          behind {e.related}
        </div>
      )}
    </div>
  );
}

function Metric({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div title={hint}>
      <div className="text-zinc-500">{label}</div>
      <div className="font-medium tabular-nums">{value}</div>
    </div>
  );
}
