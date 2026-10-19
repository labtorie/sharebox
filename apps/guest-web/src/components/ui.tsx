import { useEffect, useRef, useState, type ReactNode, type ButtonHTMLAttributes } from "react";
import {
  Archive, File, FileAudio, FileCode, FileImage, FileSpreadsheet, FileText, FileVideo, Folder, Presentation, X,
  Moon, Sun, Monitor,
} from "lucide-react";
import { fileType } from "../lib/format";

export function cx(...c: (string | false | null | undefined)[]) {
  return c.filter(Boolean).join(" ");
}

type Variant = "primary" | "secondary" | "ghost" | "danger";

export function Button({ variant = "secondary", className, children, ...rest }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
  const v: Record<Variant, string> = {
    primary: "bg-brand-600 text-white hover:bg-brand-500 shadow-sm shadow-brand-600/20",
    secondary: "bg-white text-zinc-800 ring-1 ring-zinc-200 hover:bg-zinc-50 dark:bg-zinc-900 dark:text-zinc-100 dark:ring-zinc-800 dark:hover:bg-zinc-800",
    ghost: "text-zinc-600 hover:bg-zinc-100 dark:text-zinc-300 dark:hover:bg-zinc-800",
    danger: "bg-red-600 text-white hover:bg-red-500",
  };
  return (
    <button
      {...rest}
      className={cx(
        "inline-flex items-center justify-center gap-2 rounded-xl px-3.5 py-2 text-sm font-medium transition-all duration-150",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500/60 active:scale-[0.98]",
        "disabled:pointer-events-none disabled:opacity-40",
        v[variant],
        className,
      )}
    >
      {children}
    </button>
  );
}

export function FileIcon({ name, isDir, className = "h-5 w-5" }: { name: string; isDir: boolean; className?: string }) {
  const t = fileType(name, isDir);
  const map = {
    folder: [Folder, "text-brand-500 fill-brand-500/15"],
    image: [FileImage, "text-pink-500"],
    video: [FileVideo, "text-violet-500"],
    audio: [FileAudio, "text-amber-500"],
    archive: [Archive, "text-orange-500"],
    pdf: [FileText, "text-red-500"],
    code: [FileCode, "text-emerald-500"],
    text: [FileText, "text-zinc-500"],
    sheet: [FileSpreadsheet, "text-green-600"],
    doc: [FileText, "text-blue-500"],
    slides: [Presentation, "text-orange-500"],
    file: [File, "text-zinc-400"],
  } as const;
  const [Icon, color] = map[t];
  return <Icon className={cx(className, color, "shrink-0")} strokeWidth={1.75} />;
}

export function Dialog({ open, onClose, title, children, footer }: { open: boolean; onClose: () => void; title: string; children: ReactNode; footer?: ReactNode }) {
  useEffect(() => {
    if (!open) return;
    const h = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [open, onClose]);
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center bg-zinc-950/40 p-4 backdrop-blur-sm sm:items-center" onMouseDown={onClose}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="sb-fade-in w-full max-w-md rounded-2xl bg-white p-5 shadow-2xl ring-1 ring-zinc-200 dark:bg-zinc-900 dark:ring-zinc-800"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-base font-semibold">{title}</h2>
          <button onClick={onClose} className="rounded-lg p-1 text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800" aria-label="Close">
            <X className="h-4 w-4" />
          </button>
        </div>
        {children}
        {footer && <div className="mt-5 flex justify-end gap-2">{footer}</div>}
      </div>
    </div>
  );
}

export function TextInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (props.autoFocus) ref.current?.focus();
  }, [props.autoFocus]);
  return (
    <input
      ref={ref}
      {...props}
      className={cx(
        "w-full rounded-xl bg-zinc-100 px-3.5 py-2.5 text-sm outline-none ring-1 ring-transparent transition placeholder:text-zinc-400",
        "focus:bg-white focus:ring-brand-500/50 dark:bg-zinc-800 dark:focus:bg-zinc-900",
        props.className,
      )}
    />
  );
}

export interface ToastMsg {
  id: number;
  kind: "error" | "info" | "success";
  text: string;
}

export function Toasts({ items, dismiss }: { items: ToastMsg[]; dismiss: (id: number) => void }) {
  return (
    <div className="pointer-events-none fixed inset-x-0 top-3 z-[60] flex flex-col items-center gap-2 px-3">
      {items.map((t) => (
        <div
          key={t.id}
          className={cx(
            "sb-fade-in pointer-events-auto flex max-w-lg items-start gap-3 rounded-xl px-4 py-2.5 text-sm shadow-lg ring-1",
            t.kind === "error" && "bg-red-50 text-red-800 ring-red-200 dark:bg-red-950 dark:text-red-200 dark:ring-red-900",
            t.kind === "info" && "bg-white text-zinc-800 ring-zinc-200 dark:bg-zinc-900 dark:text-zinc-100 dark:ring-zinc-800",
            t.kind === "success" && "bg-emerald-50 text-emerald-800 ring-emerald-200 dark:bg-emerald-950 dark:text-emerald-200 dark:ring-emerald-900",
          )}
        >
          <span className="flex-1">{t.text}</span>
          <button onClick={() => dismiss(t.id)} className="opacity-60 hover:opacity-100" aria-label="Dismiss">
            <X className="h-4 w-4" />
          </button>
        </div>
      ))}
    </div>
  );
}

export function useToasts() {
  const [items, setItems] = useState<ToastMsg[]>([]);
  const push = (kind: ToastMsg["kind"], text: string) => {
    const id = Date.now() + Math.random();
    setItems((x) => [...x.slice(-3), { id, kind, text }]);
    setTimeout(() => setItems((x) => x.filter((t) => t.id !== id)), kind === "error" ? 8000 : 4000);
  };
  return { items, push, dismiss: (id: number) => setItems((x) => x.filter((t) => t.id !== id)) };
}

// --- theme -----------------------------------------------------------------

export type Theme = "system" | "light" | "dark";

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (localStorage.getItem("sbx-theme") as Theme) || "system";
    } catch {
      return "system";
    }
  });
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const apply = () => document.documentElement.classList.toggle("dark", theme === "dark" || (theme === "system" && mq.matches));
    apply();
    mq.addEventListener("change", apply);
    try {
      localStorage.setItem("sbx-theme", theme);
    } catch {
      /* ignore */
    }
    return () => mq.removeEventListener("change", apply);
  }, [theme]);
  return [theme, setTheme];
}

export function ThemeToggle({ theme, setTheme }: { theme: Theme; setTheme: (t: Theme) => void }) {
  const next: Record<Theme, Theme> = { system: "light", light: "dark", dark: "system" };
  const Icon = theme === "light" ? Sun : theme === "dark" ? Moon : Monitor;
  return (
    <button
      onClick={() => setTheme(next[theme])}
      className="rounded-xl p-2 text-zinc-500 transition hover:bg-zinc-100 dark:hover:bg-zinc-800"
      title={`Theme: ${theme}`}
      aria-label="Change theme"
    >
      <Icon className="h-4.5 w-4.5" />
    </button>
  );
}

export function Logo({ className = "h-8 w-8" }: { className?: string }) {
  return (
    <svg viewBox="0 0 64 64" className={className} aria-hidden="true">
      <defs>
        <linearGradient id="sbg" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#6366f1" />
          <stop offset="1" stopColor="#06b6d4" />
        </linearGradient>
      </defs>
      <rect width="64" height="64" rx="16" fill="url(#sbg)" />
      <path d="M18 26l14-8 14 8v14l-14 8-14-8z M18 26l14 8 14-8M32 34v14" fill="none" stroke="#fff" strokeWidth="4" strokeLinejoin="round" />
    </svg>
  );
}

export function Progress({ value, className, tone = "brand" }: { value: number; className?: string; tone?: "brand" | "green" | "red" | "amber" }) {
  const c = { brand: "bg-brand-500", green: "bg-emerald-500", red: "bg-red-500", amber: "bg-amber-500" }[tone];
  return (
    <div className={cx("h-1.5 w-full overflow-hidden rounded-full bg-zinc-200 dark:bg-zinc-800", className)}>
      <div className={cx("h-full rounded-full transition-[width] duration-300", c)} style={{ width: `${Math.max(0, Math.min(100, value * 100))}%` }} />
    </div>
  );
}
