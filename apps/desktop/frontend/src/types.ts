// Mirrors internal/host.Status and internal/config.Settings (JSON).
export interface Check { state: "off" | "checking" | "ok" | "warning" | "unavailable"; detail: string }
export interface Transfer {
  id: string; guest: string; kind: "download" | "upload" | "zip"; name: string; total: number; bytes: number;
  speed: number; status: "active" | "done" | "failed" | "cancelled"; error?: string; transport: string; started: string; ended?: string;
}
export interface Guest { id: string; transport: string; route?: string; since: string; lastSeen: string; label: string }
export interface Status {
  state: "idle" | "starting" | "sharing" | "stopping";
  link?: string; linkReady: boolean; headline: string;
  server: Check; direct: Check; p2p: Check; relay: Check; mapping: Check;
  errors: string[];
  folder: { path: string; name: string; size: number; files: number; scanning: boolean; partial: boolean };
  stats: { active: Transfer[] | null; recent: Transfer[] | null; guests: Guest[] | null; upSpeed: number; downSpeed: number; totalUp: number; totalDown: number };
  started?: string; expiresAt?: string; previewUrl?: string; password: boolean;
}
export interface Settings {
  folder: string; serverUrl: string;
  permissions: { download: boolean; upload: boolean; mkdir: boolean; delete: boolean };
  maxUploadMB: number; minFreeMB: number; maxConcurrent: number; expiryHours: number;
  portMapping: boolean; keepRunningOnClose: boolean; theme: "system" | "light" | "dark"; showQr: boolean;
}
