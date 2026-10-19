// Upload and download queue with progress, cancellation and automatic resume.

import { Api, ApiError, sleep, type Entry } from "./api";
import { sha256Hex } from "./protocol";
import type { Transport } from "./transport";

export type JobKind = "upload" | "download" | "zip";
export type JobStatus = "queued" | "active" | "retrying" | "done" | "error" | "cancelled";

export interface Job {
  id: string;
  kind: JobKind;
  name: string;
  dir: string;
  size: number; // -1 unknown
  done: number;
  speed: number;
  status: JobStatus;
  error?: string;
  note?: string;
}

interface Internal extends Job {
  abort: AbortController;
  run: () => Promise<void>;
  lastBytes: number;
}

const MAX_PARALLEL_UPLOADS = 2;
const MAX_PARALLEL_DOWNLOADS = 3;
let seq = 0;

export class Transfers {
  private jobs: Internal[] = [];
  private listeners = new Set<() => void>();
  private ticker: ReturnType<typeof setInterval>;
  onUploaded: ((dir: string) => void) | null = null;

  constructor(
    private api: Api,
    private transport: () => Promise<Transport>,
  ) {
    this.ticker = setInterval(() => {
      let changed = false;
      for (const j of this.jobs) {
        if (j.status === "active") {
          j.speed = j.speed ? j.speed * 0.5 + (j.done - j.lastBytes) * 0.5 : j.done - j.lastBytes;
          j.lastBytes = j.done;
          changed = true;
        }
      }
      if (changed) this.emit();
    }, 1000);
  }

  dispose() {
    clearInterval(this.ticker);
  }

  subscribe(fn: () => void) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  snapshot(): Job[] {
    return this.jobs.map(({ abort: _a, run: _r, lastBytes: _l, ...j }) => ({ ...j }));
  }

  private emit() {
    this.listeners.forEach((l) => l());
  }

  get busy() {
    return this.jobs.some((j) => j.status === "active" || j.status === "queued" || j.status === "retrying");
  }

  private add(j: Omit<Internal, "id" | "abort" | "lastBytes" | "speed" | "done" | "status">) {
    const job: Internal = { ...j, id: "j" + ++seq, abort: new AbortController(), lastBytes: 0, speed: 0, done: 0, status: "queued" };
    this.jobs.unshift(job);
    this.emit();
    queueMicrotask(() => this.pump()); // caller finishes wiring the job first
    return job;
  }

  private pump() {
    const active = (k: (j: Internal) => boolean) => this.jobs.filter((j) => k(j) && (j.status === "active" || j.status === "retrying")).length;
    const queued = [...this.jobs].reverse().filter((j) => j.status === "queued");
    for (const j of queued) {
      const isUp = j.kind === "upload";
      const limit = isUp ? MAX_PARALLEL_UPLOADS : MAX_PARALLEL_DOWNLOADS;
      if (active((x) => (x.kind === "upload") === isUp) >= limit) continue;
      j.status = "active";
      j.run()
        .then(() => {
          if (j.status === "active" || j.status === "retrying") j.status = "done";
        })
        .catch((e: Error) => {
          if (j.abort.signal.aborted) j.status = "cancelled";
          else {
            j.status = "error";
            j.error = e.message;
          }
        })
        .finally(() => {
          j.speed = 0;
          this.emit();
          this.pump();
        });
    }
    this.emit();
  }

  cancel(id: string) {
    const j = this.jobs.find((x) => x.id === id);
    if (!j) return;
    j.abort.abort();
    if (j.status === "queued") j.status = "cancelled";
    this.emit();
  }

  retry(id: string) {
    const j = this.jobs.find((x) => x.id === id);
    if (!j || (j.status !== "error" && j.status !== "cancelled")) return;
    j.abort = new AbortController();
    j.status = "queued";
    j.error = undefined;
    j.done = 0;
    j.lastBytes = 0;
    this.pump();
  }

  clearFinished() {
    this.jobs = this.jobs.filter((j) => !["done", "cancelled", "error"].includes(j.status));
    this.emit();
  }

  // --- uploads ---------------------------------------------------------------

  upload(file: File, dir: string) {
    const job = this.add({
      kind: "upload",
      name: file.name,
      dir,
      size: file.size,
      run: () => this.runUpload(job, file),
    });
  }

  private async runUpload(j: Internal, file: File) {
    const signal = j.abort.signal;
    const create = async () => {
      const r = await this.api.raw("POST", "/api/uploads", {
        body: JSON.stringify({ dir: j.dir, name: file.name, size: file.size, modTime: file.lastModified }),
        headers: { "Content-Type": "application/json" },
        signal,
        retry: true,
      });
      const b = (await r.json()) as { id: string; chunkSize: number; error?: string };
      if (r.status !== 201) throw new ApiError(b.error ?? `Upload refused (${r.status})`, r.status);
      return b;
    };
    let { id, chunkSize } = await create();
    let offset = 0;
    let failures = 0;
    signal.addEventListener("abort", () => {
      this.api.raw("DELETE", "/api/uploads/" + id, { retry: false }).catch(() => {});
    });
    while (offset < file.size) {
      const end = Math.min(offset + chunkSize, file.size);
      const buf = await file.slice(offset, end).arrayBuffer();
      const sum = await sha256Hex(buf);
      try {
        const base = offset;
        const r = await this.api.raw("PUT", `/api/uploads/${id}?offset=${offset}`, {
          body: new Uint8Array(buf),
          headers: { "X-Chunk-SHA256": sum, "Content-Type": "application/octet-stream" },
          signal,
          retry: false,
          onUploadProgress: (n) => {
            j.done = base + n;
          },
        });
        const b = (await r.json().catch(() => ({}))) as { offset?: number; error?: string };
        if (r.status === 200) {
          offset = b.offset ?? end;
          failures = 0;
          j.status = "active";
          j.note = undefined;
        } else if (r.status === 404) {
          // The host forgot this upload (e.g. it restarted): start over.
          ({ id, chunkSize } = await create());
          offset = 0;
        } else if ((r.status === 409 || r.status === 422 || r.status >= 500) && typeof b.offset === "number" && failures < 20) {
          offset = b.offset;
          failures++;
          j.status = "retrying";
          j.note = b.error;
          await sleep(500 * failures, signal);
        } else {
          throw new ApiError(b.error ?? `Upload failed (${r.status})`, r.status);
        }
      } catch (e) {
        if (signal.aborted || e instanceof ApiError || ++failures > 20) throw e;
        // Connection lost: wait for the transport to come back and ask the
        // host where to resume.
        j.status = "retrying";
        j.note = "Connection interrupted — resuming";
        this.emit();
        await sleep(1000 * Math.min(failures, 5), signal);
        await this.transport();
        const st = await this.api.raw("GET", "/api/uploads/" + id, { signal });
        const sb = (await st.json().catch(() => ({}))) as { offset?: number };
        if (st.status === 404) {
          ({ id, chunkSize } = await create());
          offset = 0;
        } else if (typeof sb.offset === "number") offset = sb.offset;
      }
      j.done = offset;
      this.emit();
    }
    const r = await this.api.raw("POST", `/api/uploads/${id}/complete`, { signal, retry: true });
    const b = (await r.json().catch(() => ({}))) as { name?: string; error?: string };
    if (r.status !== 200) throw new ApiError(b.error ?? "Could not finish upload", r.status);
    if (b.name && b.name !== file.name) j.note = `Saved as “${b.name}”`;
    j.done = file.size;
    this.onUploaded?.(j.dir);
  }

  // --- downloads -------------------------------------------------------------

  async download(e: Entry) {
    await this.startDownload("download", e.name, e.size, this.api.downloadURI(e));
  }

  async zip(path: string, name: string) {
    await this.startDownload("zip", name + ".zip", -1, this.api.zipURI(path, name));
  }

  private async startDownload(kind: JobKind, name: string, size: number, uri: string) {
    const t = await this.transport();
    if (t.native) {
      // Direct HTTPS: let the browser's own download manager handle it
      // (with its own pause/resume). Auth uses the HttpOnly cookie.
      const a = document.createElement("a");
      a.href = uri;
      a.rel = "noopener";
      a.download = name;
      document.body.appendChild(a);
      a.click();
      a.remove();
      const job = this.add({ kind, name, dir: "", size, run: async () => {} });
      job.note = "Opened in your browser's downloads";
      return;
    }
    const job = this.add({ kind, name, dir: "", size, run: () => this.runTunnelDownload(job, uri, kind === "zip") });
  }

  private async runTunnelDownload(j: Internal, uri: string, isZip: boolean) {
    const signal = j.abort.signal;
    let received = 0;
    let reader: ReadableStreamDefaultReader<Uint8Array> | null = null;
    let failures = 0;
    const open = async () => {
      const headers: Record<string, string> = {};
      if (received > 0) headers.Range = `bytes=${received}-`;
      const res = await this.api.raw("GET", uri, { headers, signal, retry: true });
      if (res.status !== 200 && res.status !== 206) {
        const b = (await res.json().catch(() => ({}))) as { error?: string };
        throw new ApiError(b.error ?? `Download failed (${res.status})`, res.status);
      }
      if (received > 0 && res.status !== 206) throw new ApiError("The host could not resume this download", res.status);
      if (j.size < 0) {
        const cl = Number(res.headers.get("content-length") ?? "-1");
        if (cl >= 0) j.size = cl;
      }
      reader = res.body.getReader();
    };
    // Next chunk, transparently resuming with a Range request after a
    // connection drop. ZIP streams are generated on the fly and cannot be
    // resumed.
    const next = async (): Promise<Uint8Array | null> => {
      for (;;) {
        try {
          if (!reader) await open();
          const { done, value } = await reader!.read();
          if (done) return null;
          received += value.length;
          j.done = received;
          failures = 0;
          if (j.status === "retrying") {
            j.status = "active";
            j.note = undefined;
          }
          return value;
        } catch (e) {
          reader = null;
          if (signal.aborted || e instanceof ApiError || isZip || ++failures > 15) throw e;
          j.status = "retrying";
          j.note = "Connection interrupted — resuming";
          this.emit();
          await sleep(1000 * Math.min(failures, 5), signal);
        }
      }
    };

    const sw = await swController();
    if (!sw) {
      // Fallback without a service worker: buffer in memory.
      if (j.size > 1024 * 1024 * 1024) j.note = "Large file: your browser will hold it in memory until finished";
      const parts: Uint8Array<ArrayBuffer>[] = [];
      for (let c = await next(); c; c = await next()) parts.push(c as Uint8Array<ArrayBuffer>);
      const url = URL.createObjectURL(new Blob(parts));
      const a = document.createElement("a");
      a.href = url;
      a.download = j.name;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
      return;
    }

    await new Promise<void>((resolve, reject) => {
      const id = Math.random().toString(36).slice(2);
      const ch = new MessageChannel();
      const keepAlive = setInterval(() => sw.postMessage({ type: "ping" }), 10_000);
      let iframe: HTMLIFrameElement | null = null;
      const finish = (err?: Error) => {
        clearInterval(keepAlive);
        setTimeout(() => iframe?.remove(), 60_000);
        if (err) reject(err);
        else resolve();
      };
      signal.addEventListener("abort", () => {
        ch.port1.postMessage({ error: "cancelled" });
        (reader as ReadableStreamDefaultReader<Uint8Array> | null)?.cancel().catch(() => {});
        finish(new DOMException("Aborted", "AbortError"));
      });
      let started = false;
      const startTimer = setTimeout(() => {
        if (!started) {
          ch.port1.postMessage({ error: "not started" });
          finish(new Error("The download could not start; try again"));
        }
      }, 20_000);
      ch.port1.onmessage = async (m) => {
        if (m.data === "registered") {
          iframe = document.createElement("iframe");
          iframe.hidden = true;
          iframe.src = `/__sbx_dl/${id}/${encodeURIComponent(j.name)}`;
          document.body.appendChild(iframe);
          return;
        }
        if (m.data === "started") {
          started = true;
          clearTimeout(startTimer);
          return;
        }
        if (m.data === "cancel") {
          j.abort.abort();
          return;
        }
        try {
          const c = await next();
          if (!c) {
            ch.port1.postMessage({ end: true });
            finish();
            return;
          }
          const buf = c.byteOffset === 0 && c.byteLength === c.buffer.byteLength ? c.buffer : c.slice().buffer;
          ch.port1.postMessage({ chunk: buf }, [buf as ArrayBuffer]);
        } catch (e) {
          ch.port1.postMessage({ error: (e as Error).message });
          finish(e as Error);
        }
      };
      sw.postMessage({ type: "register", id, name: j.name, size: j.size }, [ch.port2]);
    });
  }
}

let swPromise: Promise<ServiceWorker | null> | null = null;

/** Registers the download service worker and waits until it controls us. */
export function swController(): Promise<ServiceWorker | null> {
  if (swPromise) return swPromise;
  swPromise = (async () => {
    if (!("serviceWorker" in navigator)) return null;
    try {
      await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      await navigator.serviceWorker.ready;
      if (navigator.serviceWorker.controller) return navigator.serviceWorker.controller;
      return await new Promise<ServiceWorker | null>((res) => {
        const t = setTimeout(() => res(null), 4000);
        navigator.serviceWorker.addEventListener("controllerchange", () => {
          clearTimeout(t);
          res(navigator.serviceWorker.controller);
        });
      });
    } catch {
      return null;
    }
  })();
  return swPromise;
}
