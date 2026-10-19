import { TunnelClient, TunnelError, type TunnelRequest, type TunnelResponse } from "./tunnel";

export type Kind = "direct" | "p2p" | "relay" | "local";

/** A way of reaching the host. All transports expose the same API. */
export interface Transport {
  kind: Kind;
  /** Native browser downloads via URL (direct HTTPS only). */
  native: boolean;
  request(req: TunnelRequest): Promise<TunnelResponse>;
  closed(): boolean;
}

function wrapFetch(res: Response): TunnelResponse {
  const headers = new Map<string, string>();
  res.headers.forEach((v, k) => headers.set(k.toLowerCase(), v));
  return {
    status: res.status,
    headers,
    body: res.body ?? new ReadableStream({ start: (c) => c.close() }),
    json: () => res.json(),
    text: () => res.text(),
  };
}

/** Direct transport: the page is served by the host itself. */
export class DirectTransport implements Transport {
  native = true;
  constructor(readonly kind: Kind) {}
  closed() {
    return false;
  }

  async request(req: TunnelRequest): Promise<TunnelResponse> {
    if (req.onUploadProgress && req.body instanceof Blob) return this.xhr(req);
    try {
      const res = await fetch(req.uri, {
        method: req.method,
        headers: req.headers,
        body: req.body as BodyInit | null | undefined,
        signal: req.signal,
        credentials: "same-origin",
        cache: "no-store",
      });
      return wrapFetch(res);
    } catch (e) {
      if ((e as Error).name === "AbortError") throw e;
      throw new TunnelError("Cannot reach the host", "network");
    }
  }

  // XHR is used for uploads because fetch has no upload progress events.
  private xhr(req: TunnelRequest): Promise<TunnelResponse> {
    return new Promise((resolve, reject) => {
      const x = new XMLHttpRequest();
      x.open(req.method, req.uri);
      for (const [k, v] of Object.entries(req.headers ?? {})) x.setRequestHeader(k, v);
      x.responseType = "text";
      x.upload.onprogress = (e) => req.onUploadProgress?.(e.loaded);
      x.onerror = () => reject(new TunnelError("Cannot reach the host", "network"));
      x.onabort = () => reject(new DOMException("Aborted", "AbortError"));
      x.onload = () => {
        const text = x.responseText;
        const headers = new Map<string, string>();
        x.getAllResponseHeaders()
          .trim()
          .split(/\r?\n/)
          .forEach((l) => {
            const i = l.indexOf(":");
            if (i > 0) headers.set(l.slice(0, i).trim().toLowerCase(), l.slice(i + 1).trim());
          });
        resolve({
          status: x.status,
          headers,
          body: new ReadableStream({ start: (c) => (c.enqueue(new TextEncoder().encode(text)), c.close()) }),
          json: async () => JSON.parse(text),
          text: async () => text,
        });
      };
      req.signal?.addEventListener("abort", () => x.abort(), { once: true });
      x.send(req.body as XMLHttpRequestBodyInit);
    });
  }
}

/** Tunnel transport over a WebRTC DataChannel (peer-to-peer or relayed). */
export class RTCTransport implements Transport {
  native = false;
  route = "";
  constructor(
    readonly kind: Kind,
    private client: TunnelClient,
    private pc: RTCPeerConnection,
  ) {}
  closed() {
    return this.client.isClosed;
  }
  request(req: TunnelRequest) {
    return this.client.request(req);
  }
  close() {
    this.client.close();
    this.pc.close();
  }
}
