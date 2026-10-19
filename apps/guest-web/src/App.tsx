import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { AlertTriangle, KeyRound, Loader2, Radio, ShieldCheck, Waypoints, Zap, Laptop, WifiOff } from "lucide-react";
import { Api, PasswordRequired, type SessionInfo } from "./lib/api";
import { Connector, type ConnState } from "./lib/connect";
import { DirectTransport, type Kind, type Transport } from "./lib/transport";
import { Transfers } from "./lib/transfers";
import { Browser } from "./components/Browser";
import { TransfersPanel } from "./components/TransfersPanel";
import { Button, Logo, TextInput, ThemeToggle, Toasts, cx, useTheme, useToasts } from "./components/ui";

const mode = document.querySelector<HTMLMetaElement>('meta[name="sharebox-mode"]')?.content === "coord" ? "coord" : "direct";

/** Reads the access key from the URL fragment (never sent to any server),
 * moves it to per-tab storage and removes it from the address bar and
 * history so it does not linger in browser history or screenshots. */
function takeKey(scope: string): string {
  const m = /(?:^#|&)k=([A-Za-z0-9_-]{20,})/.exec(location.hash);
  let key = "";
  if (m) {
    key = m[1];
    try {
      sessionStorage.setItem("sbx:" + scope, key);
    } catch {
      /* ignore */
    }
    history.replaceState(null, "", location.pathname + location.search);
  } else {
    try {
      key = sessionStorage.getItem("sbx:" + scope) ?? "";
    } catch {
      /* ignore */
    }
  }
  return key;
}

export default function App() {
  const [theme, setTheme] = useTheme();
  const sessionId = mode === "coord" ? /^\/s\/([a-z0-9]{8,64})\/?$/.exec(location.pathname)?.[1] ?? "" : "direct";
  const key = useMemo(() => (sessionId ? takeKey(sessionId) : ""), [sessionId]);

  if (mode === "coord" && !sessionId) return <Landing theme={theme} setTheme={setTheme} />;
  if (!key)
    return (
      <Shell theme={theme} setTheme={setTheme}>
        <Message icon={<AlertTriangle className="h-6 w-6 text-amber-500" />} title="This link is incomplete" text="The secret part of the link is missing. Ask the host to send you the full link again and open it exactly as received." />
      </Shell>
    );
  return <Share sessionId={sessionId} accessKey={key} theme={theme} setTheme={setTheme} />;
}

function Share({ sessionId, accessKey, theme, setTheme }: { sessionId: string; accessKey: string; theme: ReturnType<typeof useTheme>[0]; setTheme: ReturnType<typeof useTheme>[1] }) {
  const [conn, setConn] = useState<ConnState>({ phase: "connecting", step: "Connecting" });
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [needPassword, setNeedPassword] = useState<string | null>(null);
  const [pw, setPw] = useState("");
  const [authError, setAuthError] = useState<string | null>(null);
  const toasts = useToasts();
  const toast = useCallback((k: "error" | "info" | "success", t: string) => toasts.push(k, t), []); // eslint-disable-line

  const { getTransport, connector } = useMemo(() => {
    if (mode === "direct") {
      const t = new DirectTransport(location.protocol === "https:" ? "direct" : "local");
      return { getTransport: () => Promise.resolve<Transport>(t), connector: null };
    }
    const c = new Connector(sessionId, accessKey, setConn, (url) => location.replace(`${url}/#k=${accessKey}`));
    return { getTransport: () => c.transport(), connector: c };
  }, [sessionId, accessKey]);

  const api = useMemo(() => new Api(getTransport, accessKey), [getTransport, accessKey]);
  const transfers = useMemo(() => new Transfers(api, getTransport), [api, getTransport]);
  const jobs = useSyncExternalStore(
    useCallback((cb) => transfers.subscribe(cb), [transfers]),
    useMemo(() => {
      let last = transfers.snapshot();
      let lastKey = JSON.stringify(last);
      return () => {
        const s = transfers.snapshot();
        const k = JSON.stringify(s);
        if (k !== lastKey) {
          last = s;
          lastKey = k;
        }
        return last;
      };
    }, [transfers]),
  );

  useEffect(() => {
    if (connector) {
      connector.start();
      return () => connector.stop();
    }
    setConn({ phase: "connected", kind: location.protocol === "https:" ? "direct" : "local" });
  }, [connector]);

  // Warn before leaving while transfers are running.
  useEffect(() => {
    const h = (e: BeforeUnloadEvent) => {
      if (transfers.busy) e.preventDefault();
    };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [transfers]);

  const authenticate = useCallback(async () => {
    try {
      const s = await api.auth();
      setSession(s);
      setNeedPassword(null);
      setAuthError(null);
    } catch (e) {
      if (e instanceof PasswordRequired) {
        setNeedPassword(e.body.error === "wrong password" ? "Wrong password, try again." : "");
      } else setAuthError((e as Error).message);
    }
  }, [api]);

  useEffect(() => {
    if (conn.phase === "connected" && !session && needPassword === null && !authError) authenticate();
  }, [conn, session, needPassword, authError, authenticate]);

  const kind: Kind | null = conn.phase === "connected" ? conn.kind : null;

  let body: React.ReactNode;
  if (conn.phase === "failed") {
    body = (
      <Message icon={<WifiOff className="h-6 w-6 text-red-500" />} title={conn.title} text={conn.detail}>
        {conn.retryable && <Button variant="primary" onClick={() => connector?.retry()}>Try again</Button>}
      </Message>
    );
  } else if (authError) {
    body = (
      <Message icon={<AlertTriangle className="h-6 w-6 text-amber-500" />} title="Access denied" text={authError}>
        <Button onClick={() => { setAuthError(null); }}>Try again</Button>
      </Message>
    );
  } else if (needPassword !== null) {
    body = (
      <div className="sb-fade-in mx-auto mt-16 w-full max-w-sm px-4 sm:mt-24">
        <div className="rounded-3xl bg-white p-6 shadow-xl ring-1 ring-zinc-200 dark:bg-zinc-900 dark:ring-zinc-800">
          <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-2xl bg-brand-50 text-brand-600 dark:bg-brand-500/15">
            <KeyRound className="h-6 w-6" />
          </div>
          <h1 className="text-lg font-semibold">Password required</h1>
          <p className="mt-1 text-sm text-zinc-500">The host protected this share with a password.</p>
          <form
            className="mt-4 space-y-3"
            onSubmit={(e) => {
              e.preventDefault();
              api.setPassword(pw);
              setNeedPassword(null);
            }}
          >
            <TextInput type="password" autoFocus value={pw} onChange={(e) => setPw(e.target.value)} placeholder="Password" autoComplete="current-password" />
            {needPassword && <div className="text-sm text-red-600">{needPassword}</div>}
            <Button variant="primary" className="w-full" type="submit" disabled={!pw}>Unlock</Button>
          </form>
        </div>
      </div>
    );
  } else if (session && (conn.phase === "connected" || conn.phase === "reconnecting" || conn.phase === "host-offline")) {
    body = <Browser api={api} session={session} transfers={transfers} toast={toast} />;
  } else if (conn.phase === "host-offline") {
    body = (
      <Message icon={<Loader2 className="h-6 w-6 animate-spin text-zinc-400" />} title="The host is offline" text="Their computer may be asleep or disconnected. This page will connect automatically as soon as they're back." />
    );
  } else {
    body = <Connecting step={conn.phase === "connecting" ? conn.step : "Signing in"} />;
  }

  return (
    <Shell theme={theme} setTheme={setTheme} title={session?.name} badge={<ConnBadge conn={conn} kind={kind} />}>
      {(conn.phase === "reconnecting" || (conn.phase === "host-offline" && session)) && (
        <div className="flex items-center justify-center gap-2 bg-amber-50 px-4 py-2 text-sm text-amber-800 dark:bg-amber-950/60 dark:text-amber-200">
          <Loader2 className="h-4 w-4 animate-spin" />
          {conn.phase === "host-offline" ? "The host went offline — waiting for them to return…" : `${conn.reason} — reconnecting…`}
        </div>
      )}
      {body}
      <TransfersPanel jobs={jobs} onCancel={(id) => transfers.cancel(id)} onRetry={(id) => transfers.retry(id)} onClear={() => transfers.clearFinished()} />
      <Toasts items={toasts.items} dismiss={toasts.dismiss} />
    </Shell>
  );
}

function ConnBadge({ conn, kind }: { conn: ConnState; kind: Kind | null }) {
  if (conn.phase === "failed") return <Pill tone="red" icon={<WifiOff className="h-3.5 w-3.5" />} text="Not connected" />;
  if (conn.phase !== "connected" || !kind) return <Pill tone="amber" icon={<Loader2 className="h-3.5 w-3.5 animate-spin" />} text={conn.phase === "host-offline" ? "Host offline" : "Connecting"} />;
  const m: Record<Kind, [string, React.ReactNode, string]> = {
    direct: ["Direct", <Zap className="h-3.5 w-3.5" />, "Connected directly to the host over HTTPS"],
    p2p: ["Peer-to-peer", <Waypoints className="h-3.5 w-3.5" />, "Encrypted peer-to-peer connection to the host"],
    relay: ["Relayed", <Radio className="h-3.5 w-3.5" />, "Encrypted end-to-end, routed through a relay because a direct path wasn't possible"],
    local: ["Local preview", <Laptop className="h-3.5 w-3.5" />, "Host preview on this computer"],
  };
  const [text, icon, title] = m[kind];
  const route = conn.route && kind !== "relay" ? conn.route : "";
  const short = route === "local network" ? "local" : route;
  return <Pill tone="green" icon={icon} text={short ? `${text} · ${short}` : text} title={route ? `${title} (${route})` : title} />;
}

function Pill({ tone, icon, text, title }: { tone: "green" | "amber" | "red"; icon: React.ReactNode; text: string; title?: string }) {
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ring-1",
        tone === "green" && "bg-emerald-50 text-emerald-700 ring-emerald-200 dark:bg-emerald-950/60 dark:text-emerald-300 dark:ring-emerald-900",
        tone === "amber" && "bg-amber-50 text-amber-700 ring-amber-200 dark:bg-amber-950/60 dark:text-amber-300 dark:ring-amber-900",
        tone === "red" && "bg-red-50 text-red-700 ring-red-200 dark:bg-red-950/60 dark:text-red-300 dark:ring-red-900",
      )}
    >
      {icon}
      {text}
    </span>
  );
}

function Shell({ children, title, badge, theme, setTheme }: { children: React.ReactNode; title?: string; badge?: React.ReactNode; theme: ReturnType<typeof useTheme>[0]; setTheme: ReturnType<typeof useTheme>[1] }) {
  useEffect(() => {
    document.title = title ? `${title} — Sharebox` : "Sharebox";
  }, [title]);
  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center gap-3 border-b border-zinc-200/70 bg-white/70 px-4 py-3 backdrop-blur sm:px-6 dark:border-zinc-800/70 dark:bg-zinc-950/70">
        <Logo className="h-8 w-8 shrink-0" />
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold leading-tight">{title ?? "Sharebox"}</div>
          {title && <div className="text-xs text-zinc-500">Shared with you via Sharebox</div>}
        </div>
        {badge}
        <ThemeToggle theme={theme} setTheme={setTheme} />
      </header>
      <main className="flex min-h-0 flex-1 flex-col">{children}</main>
    </div>
  );
}

function Connecting({ step }: { step: string }) {
  return (
    <div className="sb-fade-in flex flex-1 flex-col items-center justify-center px-6 pb-24 text-center">
      <div className="relative mb-6 flex h-16 w-16 items-center justify-center text-brand-500">
        <span className="sb-pulse-ring absolute inset-0 rounded-full text-brand-500/30" />
        <span className="relative flex h-16 w-16 items-center justify-center rounded-full bg-brand-50 dark:bg-brand-500/15">
          <ShieldCheck className="h-7 w-7" />
        </span>
      </div>
      <div className="text-lg font-semibold">{step}…</div>
      <div className="mt-1 max-w-sm text-sm text-zinc-500">Files travel encrypted, straight from the host's computer whenever possible.</div>
    </div>
  );
}

function Message({ icon, title, text, children }: { icon: React.ReactNode; title: string; text: string; children?: React.ReactNode }) {
  return (
    <div className="sb-fade-in flex flex-1 flex-col items-center justify-center px-6 pb-24 text-center">
      <div className="mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-zinc-100 dark:bg-zinc-900">{icon}</div>
      <h1 className="text-lg font-semibold">{title}</h1>
      <p className="mt-1 max-w-md text-sm text-zinc-500">{text}</p>
      {children && <div className="mt-5">{children}</div>}
    </div>
  );
}

function Landing({ theme, setTheme }: { theme: ReturnType<typeof useTheme>[0]; setTheme: ReturnType<typeof useTheme>[1] }) {
  return (
    <Shell theme={theme} setTheme={setTheme}>
      <Message icon={<Logo className="h-8 w-8" />} title="Sharebox" text="One-click folder sharing between computers. If someone shared a folder with you, open the link they sent — no account or installation needed." />
    </Shell>
  );
}
