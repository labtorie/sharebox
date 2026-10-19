// Browser side of the DataChannel tunnel (see shared/protocol/tunnel.go).
// It multiplexes HTTP-like requests over one ordered, reliable channel with
// per-stream credit-based flow control, sequence checks and length
// verification, so the host can serve them with the same handler used for
// direct HTTPS.

import * as P from "./protocol";

/** Minimal channel abstraction (an RTCDataChannel in production). */
export interface Channel {
  send(data: Uint8Array): void;
  readonly bufferedAmount: number;
  close(): void;
}

export interface TunnelRequest {
  method: string;
  uri: string;
  headers?: Record<string, string>;
  body?: Blob | Uint8Array | string | null;
  signal?: AbortSignal;
  onUploadProgress?: (sent: number) => void;
}

export class TunnelError extends Error {
  constructor(message: string, readonly code: string = "error") {
    super(message);
  }
}

export interface TunnelResponse {
  status: number;
  headers: Map<string, string>;
  body: ReadableStream<Uint8Array>;
  json<T = unknown>(): Promise<T>;
  text(): Promise<string>;
}

const te = new TextEncoder();
const td = new TextDecoder();

class Stream {
  inQ: Uint8Array[] = [];
  inSeq = 0;
  inTotal = 0;
  inCredit = P.InitialWindow;
  unacked = 0;
  ended = false;
  error: Error | null = null;
  waiter: (() => void) | null = null;

  credit = P.InitialWindow;
  creditWaiter: (() => void) | null = null;
  outSeq = 0;
  outTotal = 0;

  resolveHead!: (h: { s: number; h: Record<string, string[]> }) => void;
  rejectHead!: (e: Error) => void;
  head: Promise<{ s: number; h: Record<string, string[]> }>;
  aborted = false;

  constructor(readonly id: number) {
    this.head = new Promise((res, rej) => {
      this.resolveHead = res;
      this.rejectHead = rej;
    });
    this.head.catch(() => {});
  }

  wake() {
    const w = this.waiter;
    this.waiter = null;
    w?.();
    const c = this.creditWaiter;
    this.creditWaiter = null;
    c?.();
  }

  fail(e: Error) {
    if (!this.error && !this.ended) {
      this.error = e;
      this.inQ = []; // buffered data must not be delivered after an abort
    }
    this.aborted = true;
    this.rejectHead(e);
    this.wake();
  }
}

export class TunnelClient {
  private streams = new Map<number, Stream>();
  private nextId = 1;
  private closed = false;
  private lowWaiters: (() => void)[] = [];
  private lastRx = Date.now();
  private hdrChain: Promise<void> = Promise.resolve();
  private timer: ReturnType<typeof setInterval>;
  onclose: ((err: Error) => void) | null = null;

  constructor(private ch: Channel) {
    this.timer = setInterval(() => {
      if (Date.now() - this.lastRx > P.DeadAfterMs) {
        this.close(new TunnelError("The host stopped responding", "dead"));
        return;
      }
      const b = new Uint8Array(8);
      new DataView(b.buffer).setFloat64(0, Date.now());
      this.sendFrame(P.FramePing, 0, 0, b).catch(() => {});
    }, P.PingIntervalMs);
  }

  get isClosed() {
    return this.closed;
  }

  /** Call when the channel's bufferedAmount drops below LowWaterMark. */
  bufferedLow() {
    const ws = this.lowWaiters;
    this.lowWaiters = [];
    ws.forEach((w) => w());
  }

  close(err: Error = new TunnelError("Connection to the host was lost", "closed")) {
    if (this.closed) return;
    this.closed = true;
    clearInterval(this.timer);
    for (const st of this.streams.values()) st.fail(err);
    this.streams.clear();
    this.bufferedLow();
    try {
      this.ch.close();
    } catch {
      /* ignore */
    }
    this.onclose?.(err);
  }

  private async sendFrame(type: number, id: number, seq: number, payload: Uint8Array) {
    while (this.ch.bufferedAmount > P.HighWaterMark) {
      if (this.closed) throw new TunnelError("Connection to the host was lost", "closed");
      await new Promise<void>((res) => {
        this.lowWaiters.push(res);
        setTimeout(res, 50); // safety net
      });
    }
    if (this.closed) throw new TunnelError("Connection to the host was lost", "closed");
    const buf = new Uint8Array(P.HeaderSize + payload.length);
    const dv = new DataView(buf.buffer);
    buf[0] = type;
    dv.setUint32(1, id);
    dv.setUint32(5, seq);
    buf.set(payload, P.HeaderSize);
    try {
      this.ch.send(buf);
    } catch (e) {
      this.close(new TunnelError("Connection to the host was lost", "closed"));
      throw e;
    }
  }

  private reset(id: number, code: string) {
    this.sendFrame(P.FrameReset, id, 0, te.encode(JSON.stringify({ c: code }))).catch(() => {});
  }

  /** Process one received message. */
  deliver(data: ArrayBuffer) {
    this.lastRx = Date.now();
    if (data.byteLength < P.HeaderSize) {
      this.close(new TunnelError("Protocol error", "protocol"));
      return;
    }
    const dv = new DataView(data);
    const type = dv.getUint8(0);
    const id = dv.getUint32(1);
    const seq = dv.getUint32(5);
    const payload = new Uint8Array(data, P.HeaderSize);
    if (id === 0) {
      if (type === P.FramePing) this.sendFrame(P.FramePong, 0, 0, payload.slice()).catch(() => {});
      return;
    }
    const st = this.streams.get(id);
    if (!st) return; // late frame for a finished stream
    switch (type) {
      case P.FrameHeaders: {
        try {
          st.resolveHead(JSON.parse(td.decode(payload)));
        } catch {
          st.fail(new TunnelError("Protocol error", "protocol"));
        }
        break;
      }
      case P.FrameData: {
        if (st.ended || st.error) return;
        if (seq !== st.inSeq) {
          st.fail(new TunnelError("Data arrived out of order; the transfer will be retried", "sequence"));
          this.reset(id, "protocol");
          this.streams.delete(id);
          return;
        }
        st.inSeq++;
        st.inCredit -= payload.length;
        if (st.inCredit < 0) {
          st.fail(new TunnelError("Host exceeded flow-control window", "protocol"));
          this.reset(id, "protocol");
          return;
        }
        st.inTotal += payload.length;
        st.inQ.push(payload.slice());
        st.wake();
        break;
      }
      case P.FrameEnd: {
        const total = Number(new DataView(data, P.HeaderSize).getBigUint64(0));
        if (total !== st.inTotal) st.error = new TunnelError(`Transfer incomplete (${st.inTotal} of ${total} bytes)`, "incomplete");
        st.ended = true;
        st.aborted = true; // stop any request body still being sent
        st.wake();
        this.streams.delete(id);
        break;
      }
      case P.FrameWindow: {
        st.credit += new DataView(data, P.HeaderSize).getUint32(0);
        st.wake();
        break;
      }
      case P.FrameReset: {
        let msg = "Transfer aborted by the host";
        let code = "reset";
        try {
          const r = JSON.parse(td.decode(payload));
          code = r.c || code;
          if (r.m) msg = r.m;
          if (r.c === "busy") msg = "The host is busy";
        } catch {
          /* ignore */
        }
        st.fail(new TunnelError(msg, code));
        this.streams.delete(id);
        break;
      }
    }
  }

  private ack(st: Stream, n: number) {
    st.inCredit += n;
    const b = new Uint8Array(4);
    new DataView(b.buffer).setUint32(0, n);
    this.sendFrame(P.FrameWindow, st.id, 0, b).catch(() => {});
  }

  private async writeBody(st: Stream, data: Uint8Array, progress: (n: number) => void) {
    let off = 0;
    while (off < data.length) {
      while (st.credit <= 0) {
        if (st.aborted || this.closed) throw st.error ?? new TunnelError("aborted", "cancel");
        await new Promise<void>((res) => (st.creditWaiter = res));
      }
      if (st.aborted) throw st.error ?? new TunnelError("aborted", "cancel");
      const n = Math.min(data.length - off, P.MaxPayload, st.credit);
      await this.sendFrame(P.FrameData, st.id, st.outSeq, data.subarray(off, off + n));
      st.outSeq++;
      st.outTotal += n;
      st.credit -= n;
      off += n;
      progress(n);
    }
  }

  private async sendBody(st: Stream, body: Blob | Uint8Array, onProgress?: (n: number) => void) {
    let sent = 0;
    const progress = (n: number) => {
      sent += n;
      onProgress?.(sent);
    };
    if (body instanceof Uint8Array) {
      await this.writeBody(st, body, progress);
    } else {
      // Read the Blob in slices so even huge files never sit in memory.
      const step = 1 << 20;
      for (let o = 0; o < body.size; o += step) {
        const chunk = new Uint8Array(await body.slice(o, o + step).arrayBuffer());
        await this.writeBody(st, chunk, progress);
      }
    }
    const end = new Uint8Array(8);
    new DataView(end.buffer).setBigUint64(0, BigInt(st.outTotal));
    await this.sendFrame(P.FrameEnd, st.id, 0, end);
  }

  async request(req: TunnelRequest): Promise<TunnelResponse> {
    if (this.closed) throw new TunnelError("Not connected to the host", "closed");
    if (this.streams.size >= P.MaxStreams) throw new TunnelError("Too many simultaneous requests", "busy");
    const id = this.nextId;
    this.nextId += 2;
    const st = new Stream(id);
    this.streams.set(id, st);

    let body: Blob | Uint8Array | null = null;
    if (typeof req.body === "string") body = te.encode(req.body);
    else if (req.body) body = req.body;
    const headers: Record<string, string[]> = {};
    for (const [k, v] of Object.entries(req.headers ?? {})) headers[k] = [v];
    if (body) headers["Content-Length"] = [String(body instanceof Blob ? body.size : body.length)];

    const onAbort = () => {
      st.fail(new DOMException("Aborted", "AbortError") as unknown as Error);
      this.reset(id, "cancel");
      this.streams.delete(id);
    };
    if (req.signal?.aborted) {
      this.streams.delete(id);
      throw new DOMException("Aborted", "AbortError");
    }
    req.signal?.addEventListener("abort", onAbort, { once: true });

    // Headers must reach the host in stream-id order, so they are chained.
    const hdr = te.encode(JSON.stringify({ m: req.method, u: req.uri, h: headers, b: !!body }));
    const sent = this.hdrChain.then(() => this.sendFrame(P.FrameHeaders, id, 0, hdr));
    this.hdrChain = sent.catch(() => {});
    await sent;
    if (body) {
      this.sendBody(st, body, req.onUploadProgress).catch((e) => {
        if (!st.aborted) {
          st.fail(e instanceof Error ? e : new Error(String(e)));
          this.reset(id, "cancel");
        }
      });
    }

    let head: { s: number; h: Record<string, string[]> };
    try {
      head = await st.head;
    } catch (e) {
      req.signal?.removeEventListener("abort", onAbort);
      throw e;
    }
    const hmap = new Map<string, string>();
    for (const [k, v] of Object.entries(head.h ?? {})) hmap.set(k.toLowerCase(), v.join(", "));

    const self = this;
    const stream = new ReadableStream<Uint8Array>(
      {
        async pull(controller) {
          for (;;) {
            if (st.inQ.length) {
              const chunk = st.inQ.shift()!;
              st.unacked += chunk.length;
              if (st.unacked >= P.InitialWindow / 4 && !st.ended) {
                self.ack(st, st.unacked);
                st.unacked = 0;
              }
              controller.enqueue(chunk);
              return;
            }
            if (st.error) {
              req.signal?.removeEventListener("abort", onAbort);
              controller.error(st.error);
              return;
            }
            if (st.ended) {
              req.signal?.removeEventListener("abort", onAbort);
              controller.close();
              return;
            }
            await new Promise<void>((res) => (st.waiter = res));
          }
        },
        cancel() {
          if (!st.ended) {
            st.fail(new TunnelError("cancelled", "cancel"));
            self.reset(id, "cancel");
          }
          self.streams.delete(id);
          req.signal?.removeEventListener("abort", onAbort);
        },
      },
      { highWaterMark: 0 },
    );

    const readAll = async () => {
      const r = stream.getReader();
      const parts: Uint8Array[] = [];
      let n = 0;
      for (;;) {
        const { done, value } = await r.read();
        if (done) break;
        parts.push(value);
        n += value.length;
      }
      const out = new Uint8Array(n);
      let o = 0;
      for (const p of parts) {
        out.set(p, o);
        o += p.length;
      }
      return out;
    };
    return {
      status: head.s,
      headers: hmap,
      body: stream,
      text: async () => td.decode(await readAll()),
      json: async () => JSON.parse(td.decode(await readAll())),
    };
  }
}

/** Adapts an RTCDataChannel to Channel and wires events. */
export function attachDataChannel(dc: RTCDataChannel): TunnelClient {
  dc.binaryType = "arraybuffer";
  dc.bufferedAmountLowThreshold = P.LowWaterMark;
  const client = new TunnelClient({
    send: (b) => dc.send(b as Uint8Array<ArrayBuffer>),
    get bufferedAmount() {
      return dc.bufferedAmount;
    },
    close: () => dc.close(),
  });
  dc.onbufferedamountlow = () => client.bufferedLow();
  dc.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      client.close(new TunnelError("Protocol error", "protocol"));
      return;
    }
    client.deliver(ev.data as ArrayBuffer);
  };
  dc.onclose = () => client.close();
  dc.onerror = () => client.close();
  return client;
}
