// Mirrors internal/host.Status and internal/config.Settings (JSON).
export interface Check { state: "off" | "checking" | "ok" | "warning" | "unavailable"; detail: string }
export interface Transfer {
  id: string; guest: string; kind: "download" | "upload" | "zip"; name: string; total: number; bytes: number;
  speed: number; status: "active" | "done" | "failed" | "cancelled"; error?: string; transport: string; started: string; ended?: string;
}
export interface Guest { id: string; transport: string; route?: string; address?: string; since: string; lastSeen: string; label: string }
export interface Endpoint { type: string; address: string; port: number; protocol: string; related?: string }
export interface PathInfo {
  kind: string; route: string; local: Endpoint; remote: Endpoint;
  rttMs: number; bytesSent: number; bytesReceived: number; cwndBytes: number; since: string;
}
export interface PeerInfo { id: string; group: string; conn: number; transport: string; connected: boolean; guests: string[] | null; path?: PathInfo }
export interface Status {
  state: "idle" | "starting" | "sharing" | "stopping";
  link?: string; linkReady: boolean; headline: string;
  server: Check; direct: Check; p2p: Check; relay: Check; mapping: Check;
  errors: string[];
  folder: { path: string; name: string; size: number; files: number; scanning: boolean; partial: boolean };
  stats: { active: Transfer[] | null; recent: Transfer[] | null; guests: Guest[] | null; upSpeed: number; downSpeed: number; totalUp: number; totalDown: number };
  started?: string; expiresAt?: string; previewUrl?: string; password: boolean;
  peers: PeerInfo[] | null; publicIp?: string;
}
export interface Settings {
  folder: string; serverUrl: string;
  permissions: { download: boolean; upload: boolean; mkdir: boolean; delete: boolean };
  maxUploadMB: number; minFreeMB: number; maxConcurrent: number; expiryHours: number;
  portMapping: boolean; keepRunningOnClose: boolean; theme: "system" | "light" | "dark"; showQr: boolean;
}
