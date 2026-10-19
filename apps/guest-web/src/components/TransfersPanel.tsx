import { useState } from "react";
import { ArrowDown, ArrowUp, Check, ChevronDown, ChevronUp, RotateCw, X, AlertCircle, FolderArchive } from "lucide-react";
import type { Job } from "../lib/transfers";
import { formatBytes, formatETA, formatSpeed } from "../lib/format";
import { Progress, cx } from "./ui";

export function TransfersPanel({ jobs, onCancel, onRetry, onClear }: { jobs: Job[]; onCancel: (id: string) => void; onRetry: (id: string) => void; onClear: () => void }) {
  const [open, setOpen] = useState(true);
  if (jobs.length === 0) return null;
  const active = jobs.filter((j) => j.status === "active" || j.status === "queued" || j.status === "retrying");
  const totalSpeed = active.reduce((s, j) => s + (j.speed || 0), 0);
  const failed = jobs.filter((j) => j.status === "error").length;
  return (
    <div className="sb-fade-in fixed inset-x-2 bottom-2 z-40 overflow-hidden rounded-2xl bg-white shadow-2xl ring-1 ring-zinc-200 sm:inset-x-auto sm:right-4 sm:bottom-4 sm:w-96 dark:bg-zinc-900 dark:ring-zinc-800">
      <button onClick={() => setOpen(!open)} className="flex w-full items-center gap-3 px-4 py-3 text-left">
        <div className="flex-1">
          <div className="text-sm font-semibold">
            {active.length > 0 ? `${active.length} transfer${active.length > 1 ? "s" : ""} in progress` : failed ? `${failed} transfer${failed > 1 ? "s" : ""} failed` : "Transfers complete"}
          </div>
          {active.length > 0 && <div className="text-xs text-zinc-500">{formatSpeed(totalSpeed) || "Starting…"}</div>}
        </div>
        {open ? <ChevronDown className="h-4 w-4 text-zinc-400" /> : <ChevronUp className="h-4 w-4 text-zinc-400" />}
      </button>
      {open && (
        <>
          <ul className="max-h-72 divide-y divide-zinc-100 overflow-y-auto border-t border-zinc-100 dark:divide-zinc-800 dark:border-zinc-800">
            {jobs.map((j) => (
              <JobRow key={j.id} j={j} onCancel={onCancel} onRetry={onRetry} />
            ))}
          </ul>
          {jobs.some((j) => ["done", "error", "cancelled"].includes(j.status)) && (
            <div className="border-t border-zinc-100 px-4 py-2 text-right dark:border-zinc-800">
              <button onClick={onClear} className="text-xs font-medium text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200">
                Clear finished
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function JobRow({ j, onCancel, onRetry }: { j: Job; onCancel: (id: string) => void; onRetry: (id: string) => void }) {
  const frac = j.size > 0 ? j.done / j.size : j.status === "done" ? 1 : 0;
  const running = j.status === "active" || j.status === "retrying" || j.status === "queued";
  const Icon = j.kind === "upload" ? ArrowUp : j.kind === "zip" ? FolderArchive : ArrowDown;
  let sub = "";
  if (j.status === "queued") sub = "Waiting…";
  else if (j.status === "retrying") sub = j.note || "Retrying…";
  else if (j.status === "active")
    sub = [j.size >= 0 ? `${formatBytes(j.done)} of ${formatBytes(j.size)}` : formatBytes(j.done), formatSpeed(j.speed), formatETA(j.size - j.done, j.speed)].filter(Boolean).join(" · ");
  else if (j.status === "done") sub = j.note || (j.size >= 0 ? formatBytes(j.size) : "Done");
  else if (j.status === "error") sub = j.error || "Failed";
  else if (j.status === "cancelled") sub = "Cancelled";
  return (
    <li className="flex items-center gap-3 px-4 py-2.5">
      <div
        className={cx(
          "flex h-8 w-8 shrink-0 items-center justify-center rounded-lg",
          j.status === "done" ? "bg-emerald-100 text-emerald-600 dark:bg-emerald-950" : j.status === "error" ? "bg-red-100 text-red-600 dark:bg-red-950" : "bg-brand-50 text-brand-600 dark:bg-brand-500/15 dark:text-brand-400",
        )}
      >
        {j.status === "done" ? <Check className="h-4 w-4" /> : j.status === "error" ? <AlertCircle className="h-4 w-4" /> : <Icon className="h-4 w-4" />}
      </div>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium" title={j.name}>
          {j.name}
        </div>
        <div className={cx("truncate text-xs", j.status === "error" ? "text-red-600 dark:text-red-400" : "text-zinc-500")}>{sub}</div>
        {running && <Progress value={frac} className="mt-1.5" tone={j.status === "retrying" ? "amber" : "brand"} />}
      </div>
      {running && (
        <button onClick={() => onCancel(j.id)} className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-100 hover:text-zinc-700 dark:hover:bg-zinc-800" aria-label="Cancel">
          <X className="h-4 w-4" />
        </button>
      )}
      {(j.status === "error" || j.status === "cancelled") && (
        <button onClick={() => onRetry(j.id)} className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-100 hover:text-zinc-700 dark:hover:bg-zinc-800" aria-label="Retry">
          <RotateCw className="h-4 w-4" />
        </button>
      )}
    </li>
  );
}
