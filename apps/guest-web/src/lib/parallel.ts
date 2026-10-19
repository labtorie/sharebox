// Parallel transfers over several WebRTC connections.
//
// Each connection has its own congestion control, so on lossy links (Wi-Fi,
// long distances) several connections together move data several times
// faster than one. Downloads fetch Range pieces over all connections and
// hand them out in order; uploads send verified chunks out of order.

import { Api, ApiError, sleep } from "./api";
import { sha256Hex } from "./protocol";
import type { Transport } from "./transport";

/** Source of live connections; the primary one comes first. */
export interface Pool {
  pool(): Transport[];
  /** How many connections the host allows (1 = no parallel support). */
  parallel(): number;
  /** Waits for the primary connection (used when every connection is down). */
  primary(): Promise<Transport>;
}

export const PIECE = 4 << 20; // download piece size
export const MIN_PARALLEL_SIZE = 16 << 20; // smaller files gain little

let seq = 0;

/** Hands out a file's bytes in order while fetching pieces in parallel. */
export class ParallelDownload {
  private pieces = new Map<number, Promise<Uint8Array>>();
  private nextToFetch = 0;
  private nextToEmit = 0;
  private rr = 0;
  private readonly count: number;
  private readonly group = `p${Date.now().toString(36)}${++seq}`;

  constructor(
    private api: Api,
    private uri: string,
    private size: number,
    private conns: Pool,
    private signal: AbortSignal,
    private onBytes: (n: number) => void,
  ) {
    this.count = Math.ceil(size / PIECE);
  }

  /** Next chunk in file order, or null at the end. */
  async next(): Promise<Uint8Array | null> {
    if (this.nextToEmit >= this.count) return null;
    this.fill();
    const i = this.nextToEmit;
    const data = await this.pieces.get(i)!;
    this.pieces.delete(i);
    this.nextToEmit++;
    this.fill();
    return data;
  }

  /** Keeps about two pieces in flight per connection (bounded memory). */
  private fill() {
    const ahead = Math.max(2, this.conns.pool().length * 2);
    while (this.nextToFetch < this.count && this.nextToFetch - this.nextToEmit < ahead) {
      const i = this.nextToFetch++;
      const p = this.fetchPiece(i);
      p.catch(() => {}); // surfaced when awaited in next()
      this.pieces.set(i, p);
    }
  }

  private async pick(): Promise<Transport> {
    const pool = this.conns.pool();
    if (pool.length === 0) return this.conns.primary();
    return pool[this.rr++ % pool.length];
  }

  private async fetchPiece(i: number): Promise<Uint8Array> {
    const start = i * PIECE;
    const end = Math.min(start + PIECE, this.size) - 1;
    for (let attempt = 0; ; attempt++) {
      if (this.signal.aborted) throw new DOMException("Aborted", "AbortError");
      const transport = await this.pick();
      try {
        const res = await this.api.raw("GET", this.uri, {
          headers: { Range: `bytes=${start}-${end}`, "X-Sharebox-Transfer": this.group },
          signal: this.signal,
          transport,
          retry: false,
        });
        if (res.status !== 206) {
          const b = (await res.json().catch(() => ({}))) as { error?: string };
          throw new ApiError(b.error ?? `Download failed (${res.status})`, res.status);
        }
        const buf = new Uint8Array(end - start + 1);
        let off = 0;
        const r = res.body.getReader();
        for (;;) {
          const { done, value } = await r.read();
          if (done) break;
          if (off + value.length > buf.length) throw new Error("host sent too much data");
          buf.set(value, off);
          off += value.length;
          this.onBytes(value.length);
        }
        if (off !== buf.length) throw new Error("piece incomplete");
        return buf;
      } catch (e) {
        if (this.signal.aborted || (e instanceof ApiError && e.status !== 429 && e.status < 500) || attempt >= 8) throw e;
        await sleep(300 * (attempt + 1), this.signal);
      }
    }
  }
}

/**
 * Uploads a file as verified chunks over all connections at once. Chunks may
 * arrive in any order; the host assembles them and completes the file once
 * every chunk is in. Returns the stored name.
 */
export async function parallelUpload(
  api: Api,
  conns: Pool,
  file: File,
  dir: string,
  signal: AbortSignal,
  onProgress: (done: number) => void,
): Promise<string | undefined> {
  const r = await api.raw("POST", "/api/uploads", {
    body: JSON.stringify({ dir, name: file.name, size: file.size, modTime: file.lastModified, parallel: true }),
    headers: { "Content-Type": "application/json" },
    signal,
    retry: true,
  });
  const created = (await r.json()) as { id: string; chunkSize: number; error?: string };
  if (r.status !== 201) throw new ApiError(created.error ?? `Upload refused (${r.status})`, r.status);
  const { id, chunkSize } = created;
  signal.addEventListener("abort", () => {
    api.raw("DELETE", "/api/uploads/" + id, { retry: false }).catch(() => {});
  });

  const total = Math.ceil(file.size / chunkSize);
  const queue = Array.from({ length: total }, (_, i) => i);
  const failures = new Map<number, number>();
  const inflight = new Map<number, number>(); // chunk -> bytes sent so far
  let doneBytes = 0;
  const report = () => onProgress(doneBytes + [...inflight.values()].reduce((a, b) => a + b, 0));

  const worker = async (w: number) => {
    while (queue.length) {
      if (signal.aborted) throw new DOMException("Aborted", "AbortError");
      const i = queue.shift()!;
      const off = i * chunkSize;
      const blob = file.slice(off, Math.min(off + chunkSize, file.size));
      const buf = new Uint8Array(await blob.arrayBuffer());
      const sum = await sha256Hex(buf.buffer);
      const pool = conns.pool();
      const transport = pool.length ? pool[w % pool.length] : await conns.primary();
      inflight.set(i, 0);
      try {
        const res = await api.raw("PUT", `/api/uploads/${id}?offset=${off}`, {
          body: buf,
          headers: { "X-Chunk-SHA256": sum, "Content-Type": "application/octet-stream" },
          signal,
          transport,
          retry: false,
          onUploadProgress: (n) => {
            inflight.set(i, n);
            report();
          },
        });
        const b = (await res.json().catch(() => ({}))) as { error?: string };
        if (res.status === 200) {
          doneBytes += buf.length;
        } else if (res.status === 404 || res.status === 400 || res.status === 403 || res.status === 507) {
          throw new ApiError(b.error ?? `Upload failed (${res.status})`, res.status);
        } else {
          throw new Error(b.error ?? `HTTP ${res.status}`); // retried below
        }
      } catch (e) {
        if (signal.aborted || e instanceof ApiError) throw e;
        const n = (failures.get(i) ?? 0) + 1;
        failures.set(i, n);
        if (n > 10) throw e;
        queue.push(i); // try again, possibly on another connection
        await sleep(500 * Math.min(n, 6), signal);
      } finally {
        inflight.delete(i);
        report();
      }
    }
  };
  const workers = Math.max(1, Math.min(conns.parallel(), total));
  await Promise.all(Array.from({ length: workers }, (_, w) => worker(w)));

  const done = await api.raw("POST", `/api/uploads/${id}/complete`, { signal, retry: true });
  const b = (await done.json().catch(() => ({}))) as { name?: string; error?: string };
  if (done.status !== 200) throw new ApiError(b.error ?? "Could not finish upload", done.status);
  return b.name;
}
