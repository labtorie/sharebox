import { useCallback, useEffect, useState } from "react";
import {
  ArrowDown, ArrowUp, Check, Copy, ExternalLink, FolderOpen, Globe, KeyRound, Loader2, Power, QrCode, Radio, RefreshCw,
  Router, Server, Settings as SettingsIcon, ShieldAlert, Users, Waypoints, X, Zap, AlertTriangle, FolderArchive,
} from "lucide-react";
import * as Go from "../wailsjs/go/main/App";
import { Environment, EventsOn } from "../wailsjs/runtime/runtime";
import type { Check as CheckT, Settings, Status, Transfer } from "./types";
import { GuestRow, peerFor } from "./RouteDetails";
import { Button, Dialog, FileIcon, Logo, Progress, TextInput, cx } from "./ui";
import { formatBytes, formatSpeed } from "./format";

const idle: Status = {
  state: "idle", linkReady: false, headline: "Not sharing", errors: [], password: false, peers: [],
  server: { state: "off", detail: "" }, direct: { state: "off", detail: "" }, p2p: { state: "off", detail: "" },
  relay: { state: "off", detail: "" }, mapping: { state: "off", detail: "" },
  folder: { path: "", name: "", size: 0, files: 0, scanning: false, partial: false },
  stats: { active: [], recent: [], guests: [], upSpeed: 0, downSpeed: 0, totalUp: 0, totalDown: 0 },
};

function errText(e: unknown) {
  return typeof e === "string" ? e : (e as Error)?.message ?? String(e);
}

export default function App() {
  const [status, setStatus] = useState<Status>(idle);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const [password, setPassword] = useState("");
  const [copied, setCopied] = useState(false);
  const [qr, setQr] = useState<string | null>(null);
  const [confirmRegen, setConfirmRegen] = useState(false);
  // macOS draws the window buttons inside our header (hidden-inset title bar).
  const [isMac, setIsMac] = useState(navigator.userAgent.includes("Mac"));
  useEffect(() => {
    Environment().then((e) => setIsMac(e.platform === "darwin")).catch(() => {});
  }, []);

  useEffect(() => {
    Go.GetSettings().then((s) => setSettings(s as unknown as Settings));
    Go.Status().then((s) => setStatus(s as unknown as Status));
    return EventsOn("status", (s: Status) => setStatus(s));
  }, []);

  // Theme from settings.
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const apply = () => {
      const t = settings?.theme ?? "system";
      document.documentElement.classList.toggle("dark", t === "dark" || (t === "system" && mq.matches));
    };
    apply();
    mq.addEventListener("change", apply);
    return () => mq.removeEventListener("change", apply);
  }, [settings?.theme]);

  const save = useCallback(async (s: Settings) => {
    setSettings(s);
    try {
      await Go.SaveSettings(s as never);
    } catch (e) {
      setError(errText(e));
    }
  }, []);

  const running = status.state !== "idle";
  const sharing = status.state === "sharing";

  const start = async () => {
    setError(null);
    setBusy(true);
    try {
      await Go.Start(password);
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(false);
    }
  };
  const stop = async () => {
    setBusy(true);
    await Go.Stop();
    setBusy(false);
    setQr(null);
  };
  const choose = async () => {
    try {
      const d = await Go.ChooseFolder();
      if (d) setSettings((await Go.GetSettings()) as unknown as Settings);
    } catch (e) {
      setError(errText(e));
    }
  };
  const copy = async () => {
    try {
      await Go.CopyLink();
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch (e) {
      setError(errText(e));
    }
  };
  useEffect(() => {
    if (qr !== null && status.link) Go.QRCode(status.link).then(setQr);
  }, [status.link]); // eslint-disable-line

  if (!settings) return null;
  const folderName = settings.folder ? settings.folder.split(/[\\/]/).filter(Boolean).pop() : "";
  const guests = status.stats.guests ?? [];
  const active = status.stats.active ?? [];
  const recent = status.stats.recent ?? [];

  return (
    <div className="flex h-full flex-col">
      {/* Title bar (draggable; leaves room for macOS traffic lights) */}
      <header className={cx("drag flex h-14 shrink-0 items-center gap-3 border-b border-zinc-200/70 pr-4 dark:border-zinc-800/70", isMac ? "pl-24" : "pl-4")}>
        <Logo className="h-7 w-7" />
        <span className="text-[15px] font-semibold tracking-tight">Sharebox</span>
        <StatusPill status={status} />
        <div className="flex-1" />
        <button onClick={() => setShowSettings(true)} className="no-drag rounded-xl p-2 text-zinc-500 hover:bg-zinc-200/60 dark:hover:bg-zinc-800" aria-label="Settings">
          <SettingsIcon className="h-4.5 w-4.5" />
        </button>
      </header>

      {error && (
        <div className="sb-fade-in flex items-start gap-3 bg-red-50 px-5 py-2.5 text-sm text-red-800 dark:bg-red-950/70 dark:text-red-200">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <span className="selectable flex-1">{error}</span>
          <button onClick={() => setError(null)} aria-label="Dismiss"><X className="h-4 w-4" /></button>
        </div>
      )}

      <div className="grid min-h-0 flex-1 grid-cols-1 content-start gap-4 overflow-y-auto overflow-x-hidden p-5 lg:grid-cols-[minmax(0,1.25fr)_minmax(0,1fr)]">
        {/* Left column */}
        <div className="flex min-w-0 flex-col gap-4">
          <Card>
            <div className="flex items-center gap-4">
              <div className="flex h-14 w-14 shrink-0 items-center justify-center rounded-2xl bg-brand-50 dark:bg-brand-500/15">
                <FileIcon name="" isDir className="h-7 w-7" />
              </div>
              <div className="min-w-0 flex-1">
                <div className="truncate text-base font-semibold">{folderName || "No folder selected"}</div>
                <div className="selectable truncate text-xs text-zinc-500" title={settings.folder}>{settings.folder || "Choose the folder you want to share"}</div>
                {running && (
                  <div className="mt-0.5 text-xs text-zinc-500">
                    {status.folder.files.toLocaleString()} files · {formatBytes(status.folder.size)}
                    {status.folder.scanning && " (counting…)"}
                  </div>
                )}
              </div>
              <div className="flex gap-2">
                {settings.folder && (
                  <Button variant="ghost" onClick={() => Go.RevealFolder().catch((e) => setError(errText(e)))} title="Show in Finder/Explorer"><FolderOpen className="h-4 w-4" /></Button>
                )}
                <Button onClick={choose} disabled={running}>{settings.folder ? "Change" : "Choose folder"}</Button>
              </div>
            </div>
          </Card>

          {!running && (
            <Card>
              <label className="mb-2 flex items-center gap-2 text-xs font-medium text-zinc-500">
                <KeyRound className="h-3.5 w-3.5" /> Optional password for this share
              </label>
              <TextInput type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="No password" />
            </Card>
          )}

          <button
            onClick={running ? stop : start}
            disabled={busy || (!running && !settings.folder) || status.state === "stopping"}
            className={cx(
              "group relative flex h-20 items-center justify-center gap-3 rounded-3xl text-lg font-semibold text-white shadow-lg transition-all duration-200 active:scale-[0.99] disabled:opacity-50",
              running ? "bg-zinc-800 shadow-zinc-900/20 hover:bg-zinc-700 dark:bg-zinc-800" : "bg-gradient-to-br from-brand-500 to-cyan-500 shadow-brand-500/30 hover:brightness-110",
            )}
          >
            {busy || status.state === "starting" || status.state === "stopping" ? <Loader2 className="h-6 w-6 animate-spin" /> : <Power className="h-6 w-6" />}
            {running ? "Stop Sharing" : "Start Sharing"}
          </button>

          {running && (
            <Card>
              <div className="mb-2 flex items-center justify-between">
                <span className="text-xs font-medium text-zinc-500">Share link</span>
                {status.expiresAt && <span className="text-xs text-zinc-500">Expires {new Date(status.expiresAt).toLocaleString()}</span>}
              </div>
              {status.linkReady && status.link ? (
                <>
                  <div className="flex gap-2">
                    <div className="selectable min-w-0 flex-1 truncate rounded-xl bg-zinc-100 px-3.5 py-2.5 font-mono text-[13px] dark:bg-zinc-800" title={status.link}>
                      {status.link}
                    </div>
                    <Button variant="primary" onClick={copy} className="w-28">
                      {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                      {copied ? "Copied" : "Copy"}
                    </Button>
                  </div>
                  <div className="mt-3 flex flex-wrap gap-2">
                    <Button variant="ghost" onClick={() => (qr !== null ? setQr(null) : Go.QRCode(status.link!).then(setQr))}>
                      <QrCode className="h-4 w-4" /> {qr !== null ? "Hide QR code" : "QR code"}
                    </Button>
                    <Button variant="ghost" onClick={() => Go.OpenPreview()}><ExternalLink className="h-4 w-4" /> Preview as guest</Button>
                    <Button variant="ghost" onClick={() => setConfirmRegen(true)}><RefreshCw className="h-4 w-4" /> New link</Button>
                  </div>
                  {qr && (
                    <div className="sb-fade-in mt-3 flex justify-center">
                      <img src={qr} alt="QR code for the share link" className="h-48 w-48 rounded-2xl bg-white p-2 ring-1 ring-zinc-200" />
                    </div>
                  )}
                  <p className="mt-3 text-xs text-zinc-500">Anyone with this link can access the folder{status.password ? " (password required)" : ""}. It contains a secret key — share it privately.</p>
                </>
              ) : (
                <div className="flex items-center gap-3 rounded-xl bg-zinc-100 px-3.5 py-3 text-sm text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  {status.headline} — the link appears once a connection method is verified.
                </div>
              )}
            </Card>
          )}

          {running && (
            <Card title="Connectivity">
              <CheckRow icon={<Server className="h-4 w-4" />} label="Sharebox server" c={status.server} />
              <CheckRow icon={<Zap className="h-4 w-4" />} label="Direct (HTTPS)" c={status.direct} />
              <CheckRow icon={<Waypoints className="h-4 w-4" />} label="Peer-to-peer" c={status.p2p} />
              <CheckRow icon={<Radio className="h-4 w-4" />} label="Relay" c={status.relay} />
              <CheckRow icon={<Router className="h-4 w-4" />} label="Router port mapping" c={status.mapping} />
            </Card>
          )}

          {status.errors.length > 0 && (
            <Card title="Problems" action={<button className="text-xs text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200" onClick={() => Go.ClearErrors()}>Clear</button>}>
              <ul className="selectable space-y-1 text-xs text-zinc-600 dark:text-zinc-400">
                {status.errors.slice().reverse().map((e, i) => <li key={i}>{e}</li>)}
              </ul>
            </Card>
          )}
        </div>

        {/* Right column */}
        <div className="flex min-w-0 flex-col gap-4">
          <Card title="Guests can">
            <Toggle label="Download files" checked={settings.permissions.download} onChange={(v) => save({ ...settings, permissions: { ...settings.permissions, download: v } })} />
            <Toggle label="Upload files" checked={settings.permissions.upload} onChange={(v) => save({ ...settings, permissions: { ...settings.permissions, upload: v } })} />
            <Toggle label="Create folders" checked={settings.permissions.mkdir} onChange={(v) => save({ ...settings, permissions: { ...settings.permissions, mkdir: v } })} />
            <Toggle
              label="Delete files and folders"
              hint="Guests could permanently delete anything in this folder."
              danger
              checked={settings.permissions.delete}
              onChange={(v) => save({ ...settings, permissions: { ...settings.permissions, delete: v } })}
            />
            {sharing && <PasswordEditor has={status.password} onSet={(pw) => Go.SetPassword(pw).catch((e) => setError(errText(e)))} />}
          </Card>

          {running && (
            <Card title="Activity">
              <div className="mb-3 grid grid-cols-2 gap-3">
                <Stat icon={<ArrowDown className="h-4 w-4 text-brand-500" />} label="Sending" value={formatSpeed(status.stats.downSpeed) || "0 B/s"} sub={`${formatBytes(status.stats.totalDown)} total`} />
                <Stat icon={<ArrowUp className="h-4 w-4 text-cyan-500" />} label="Receiving" value={formatSpeed(status.stats.upSpeed) || "0 B/s"} sub={`${formatBytes(status.stats.totalUp)} total`} />
              </div>
              <div className="mb-1 flex items-center gap-2 text-xs font-medium text-zinc-500"><Users className="h-3.5 w-3.5" /> Connected guests ({guests.length})</div>
              {guests.length === 0 ? (
                <div className="py-2 text-sm text-zinc-500">No one is connected yet.</div>
              ) : (
                <ul className="mb-2 divide-y divide-zinc-100 dark:divide-zinc-800">
                  {guests.map((g) => (
                    <GuestRow key={g.id} g={g} peers={peerFor(status, g.id)} />
                  ))}
                </ul>
              )}
              {(active.length > 0 || recent.length > 0) && <div className="mt-2 mb-1 text-xs font-medium text-zinc-500">Transfers</div>}
              <ul className="space-y-2">
                {active.map((t) => <TransferRow key={t.id} t={t} />)}
                {recent.slice(0, 12).map((t) => <TransferRow key={t.id} t={t} />)}
              </ul>
            </Card>
          )}
          {!running && (
            <Card>
              <div className="flex items-start gap-3 text-sm text-zinc-600 dark:text-zinc-400">
                <Globe className="mt-0.5 h-5 w-5 shrink-0 text-brand-500" />
                <div>
                  <div className="font-medium text-zinc-900 dark:text-zinc-100">How it works</div>
                  Pick a folder and press Start Sharing. Sharebox connects automatically — directly when possible, otherwise through an encrypted relay — and gives you a link. Your friend opens it in any browser; no installation or account needed.
                  {!settings.serverUrl && (
                    <div className="mt-2 rounded-xl bg-amber-50 p-3 text-amber-800 dark:bg-amber-950/60 dark:text-amber-200">
                      No Sharebox server is configured yet. <button className="font-medium underline" onClick={() => setShowSettings(true)}>Open Settings</button> to add one.
                    </div>
                  )}
                </div>
              </div>
            </Card>
          )}
        </div>
      </div>

      <SettingsDialog open={showSettings} onClose={() => setShowSettings(false)} settings={settings} onSave={save} running={running} />
      <Dialog
        open={confirmRegen}
        onClose={() => setConfirmRegen(false)}
        title="Create a new link?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRegen(false)}>Cancel</Button>
            <Button variant="primary" onClick={async () => { setConfirmRegen(false); setQr(null); try { await Go.RegenerateLink(); } catch (e) { setError(errText(e)); } }}>Create new link</Button>
          </>
        }
      >
        <p className="text-sm text-zinc-600 dark:text-zinc-400">The current link stops working immediately and everyone connected is disconnected. You'll need to send the new link again.</p>
      </Dialog>
    </div>
  );
}

function Card({ title, action, children }: { title?: string; action?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="sb-fade-in rounded-2xl bg-white p-4 shadow-sm ring-1 ring-zinc-200/80 dark:bg-zinc-900 dark:ring-zinc-800">
      {title && (
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold">{title}</h2>
          {action}
        </div>
      )}
      {children}
    </section>
  );
}

function StatusPill({ status }: { status: Status }) {
  const tone =
    status.state === "idle" ? "zinc" : status.linkReady && status.headline.startsWith("Sharing") ? (status.headline.includes("may fail") ? "amber" : "green") : status.headline.startsWith("Offline") || status.headline.startsWith("No working") ? "red" : "amber";
  const cls = {
    zinc: "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300",
    green: "bg-emerald-50 text-emerald-700 dark:bg-emerald-950/70 dark:text-emerald-300",
    amber: "bg-amber-50 text-amber-700 dark:bg-amber-950/70 dark:text-amber-300",
    red: "bg-red-50 text-red-700 dark:bg-red-950/70 dark:text-red-300",
  }[tone];
  const dot = { zinc: "bg-zinc-400", green: "bg-emerald-500", amber: "bg-amber-500 sb-pulse", red: "bg-red-500" }[tone];
  return (
    <span className={cx("no-drag inline-flex max-w-[28rem] items-center gap-2 truncate rounded-full px-3 py-1 text-xs font-medium", cls)} title={status.headline}>
      <span className={cx("h-2 w-2 shrink-0 rounded-full", dot)} />
      <span className="truncate">{status.headline}</span>
    </span>
  );
}

function CheckRow({ icon, label, c }: { icon: React.ReactNode; label: string; c: CheckT }) {
  const m: Record<CheckT["state"], [string, React.ReactNode]> = {
    ok: ["text-emerald-600", <Check className="h-4 w-4" />],
    warning: ["text-amber-600", <AlertTriangle className="h-4 w-4" />],
    unavailable: ["text-zinc-400", <X className="h-4 w-4" />],
    checking: ["text-brand-500", <Loader2 className="h-4 w-4 animate-spin" />],
    off: ["text-zinc-400", <span className="text-xs">—</span>],
  };
  const [color, mark] = m[c.state] ?? m.off;
  return (
    <div className="flex items-start gap-3 border-t border-zinc-100 py-2 first:border-t-0 first:pt-0 dark:border-zinc-800">
      <span className="mt-0.5 text-zinc-400">{icon}</span>
      <div className="min-w-0 flex-1">
        <div className="text-sm">{label}</div>
        {c.detail && <div className="selectable text-xs text-zinc-500">{c.detail}</div>}
      </div>
      <span className={cx("mt-0.5", color)}>{mark}</span>
    </div>
  );
}

function Toggle({ label, hint, checked, onChange, danger }: { label: string; hint?: string; checked: boolean; onChange: (v: boolean) => void; danger?: boolean }) {
  return (
    <label className="flex cursor-pointer items-center gap-3 py-1.5">
      <div className="flex-1">
        <div className="flex items-center gap-1.5 text-sm">
          {danger && checked && <ShieldAlert className="h-4 w-4 text-red-500" />}
          {label}
        </div>
        {hint && checked && <div className="text-xs text-red-600 dark:text-red-400">{hint}</div>}
      </div>
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={cx("relative h-6 w-10 shrink-0 rounded-full transition-colors", checked ? (danger ? "bg-red-500" : "bg-brand-600") : "bg-zinc-300 dark:bg-zinc-700")}
      >
        <span className={cx("absolute top-0.5 left-0.5 h-5 w-5 rounded-full bg-white shadow transition-transform", checked && "translate-x-4")} />
      </button>
    </label>
  );
}

function PasswordEditor({ has, onSet }: { has: boolean; onSet: (pw: string) => void }) {
  const [editing, setEditing] = useState(false);
  const [pw, setPw] = useState("");
  return (
    <div className="mt-2 border-t border-zinc-100 pt-3 dark:border-zinc-800">
      <div className="flex items-center gap-2 text-sm">
        <KeyRound className="h-4 w-4 text-zinc-400" />
        <span className="flex-1">{has ? "Password protected" : "No password"}</span>
        {!editing && <Button variant="ghost" onClick={() => setEditing(true)}>{has ? "Change" : "Add"}</Button>}
        {has && !editing && <Button variant="ghost" onClick={() => onSet("")}>Remove</Button>}
      </div>
      {editing && (
        <div className="mt-2 flex gap-2">
          <TextInput type="password" autoFocus value={pw} onChange={(e) => setPw(e.target.value)} placeholder="New password" />
          <Button variant="primary" disabled={!pw} onClick={() => { onSet(pw); setPw(""); setEditing(false); }}>Save</Button>
        </div>
      )}
      <p className="mt-1 text-xs text-zinc-500">Applies to new sign-ins. Already-connected guests keep access until you create a new link.</p>
    </div>
  );
}

function Stat({ icon, label, value, sub }: { icon: React.ReactNode; label: string; value: string; sub: string }) {
  return (
    <div className="rounded-xl bg-zinc-50 p-3 dark:bg-zinc-800/60">
      <div className="flex items-center gap-1.5 text-xs text-zinc-500">{icon}{label}</div>
      <div className="mt-1 text-base font-semibold tabular-nums">{value}</div>
      <div className="text-xs text-zinc-500">{sub}</div>
    </div>
  );
}

function TransportBadge({ t }: { t: string }) {
  const m: Record<string, [string, string]> = {
    direct: ["Direct", "bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300"],
    p2p: ["P2P", "bg-brand-50 text-brand-700 dark:bg-brand-500/15 dark:text-brand-300"],
    relay: ["Relay", "bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-300"],
    local: ["Preview", "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300"],
  };
  const [label, cls] = m[t] ?? [t, "bg-zinc-100"];
  return <span className={cx("rounded-md px-1.5 py-0.5 text-[11px] font-semibold", cls)}>{label}</span>;
}

function TransferRow({ t }: { t: Transfer }) {
  const frac = t.total > 0 ? t.bytes / t.total : 0;
  const Icon = t.kind === "upload" ? ArrowUp : t.kind === "zip" ? FolderArchive : ArrowDown;
  return (
    <li className="flex items-center gap-3">
      <Icon className={cx("h-4 w-4 shrink-0", t.status === "failed" ? "text-red-500" : t.status === "done" ? "text-emerald-500" : "text-brand-500")} />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-sm">{t.name}</span>
          <TransportBadge t={t.transport} />
        </div>
        {t.status === "active" ? (
          <>
            <Progress value={t.total > 0 ? frac : 0} className="mt-1" />
            <div className="mt-0.5 text-xs text-zinc-500 tabular-nums">
              {formatBytes(t.bytes)}{t.total > 0 && ` of ${formatBytes(t.total)}`} · {formatSpeed(t.speed)}
            </div>
          </>
        ) : (
          <div className={cx("text-xs", t.status === "failed" ? "text-red-600" : "text-zinc-500")}>
            {t.status === "done" ? `${formatBytes(t.bytes)} · done` : t.status === "cancelled" ? "Cancelled" : t.error || "Failed"}
          </div>
        )}
      </div>
      {t.status === "active" && (
        <button onClick={() => Go.CancelTransfer(t.id)} className="rounded-lg p-1 text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800" title="Cancel">
          <X className="h-4 w-4" />
        </button>
      )}
    </li>
  );
}

function SettingsDialog({ open, onClose, settings, onSave, running }: { open: boolean; onClose: () => void; settings: Settings; onSave: (s: Settings) => Promise<void>; running: boolean }) {
  const [s, setS] = useState(settings);
  const [def, setDef] = useState("");
  useEffect(() => {
    Go.DefaultServer().then(setDef);
  }, []);
  useEffect(() => setS(settings), [settings, open]);
  const num = (k: keyof Settings) => (e: React.ChangeEvent<HTMLInputElement>) => setS({ ...s, [k]: Math.max(0, Number(e.target.value) || 0) });
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Settings"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={async () => { await onSave(s); onClose(); }}>Save</Button>
        </>
      }
    >
      <div className="max-h-[60vh] space-y-4 overflow-y-auto pr-1">
        <Field
          label="Sharebox server"
          hint={
            (def && s.serverUrl === def ? "Built-in shared server — no change needed. " : def ? "Only change this if you run your own server. " : "The coordination server that creates links and relays connections. ") +
            (running ? "Takes effect next time you start sharing." : "")
          }
        >
          <div className="flex gap-2">
            <TextInput value={s.serverUrl} onChange={(e) => setS({ ...s, serverUrl: e.target.value })} placeholder={def || "https://share.example.org"} spellCheck={false} />
            {def && s.serverUrl !== def && (
              <Button variant="ghost" onClick={() => setS({ ...s, serverUrl: def })} title={def}>Use default</Button>
            )}
          </div>
        </Field>
        <Field label="Appearance">
          <select value={s.theme} onChange={(e) => setS({ ...s, theme: e.target.value as Settings["theme"] })} className="w-full rounded-xl bg-zinc-100 px-3 py-2.5 text-sm dark:bg-zinc-800">
            <option value="system">Match system</option>
            <option value="light">Light</option>
            <option value="dark">Dark</option>
          </select>
        </Field>
        <Toggle label="Keep sharing when the window is closed" checked={s.keepRunningOnClose} onChange={(v) => setS({ ...s, keepRunningOnClose: v })} />
        <Toggle label="Automatically configure the router (UPnP / NAT-PMP / PCP)" checked={s.portMapping} onChange={(v) => setS({ ...s, portMapping: v })} />
        <div className="grid grid-cols-2 gap-3">
          <Field label="Link expires after (hours, 0 = never)"><TextInput type="number" min={0} value={s.expiryHours} onChange={num("expiryHours")} /></Field>
          <Field label="Max upload size (MB, 0 = none)"><TextInput type="number" min={0} value={s.maxUploadMB} onChange={num("maxUploadMB")} /></Field>
          <Field label="Keep free on disk (MB)"><TextInput type="number" min={0} value={s.minFreeMB} onChange={num("minFreeMB")} /></Field>
          <Field label="Simultaneous transfers"><TextInput type="number" min={1} max={64} value={s.maxConcurrent} onChange={num("maxConcurrent")} /></Field>
        </div>
        {running && <p className="text-xs text-zinc-500">Limits apply immediately; server, router and expiry changes apply on the next start.</p>}
      </div>
    </Dialog>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="mb-1.5 text-xs font-medium text-zinc-500">{label}</div>
      {children}
      {hint && <div className="mt-1 text-xs text-zinc-500">{hint}</div>}
    </div>
  );
}
