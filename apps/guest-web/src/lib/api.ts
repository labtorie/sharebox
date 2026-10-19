import type { Transport } from "./transport";
import type { TunnelResponse } from "./tunnel";

export interface Entry {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  modTime: string;
}

export interface Permissions {
  download: boolean;
  upload: boolean;
  mkdir: boolean;
  delete: boolean;
}

export interface SessionInfo {
  name: string;
  permissions: Permissions;
  maxUploadBytes: number;
  chunkSize: number;
  expiresAt?: string;
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body: Record<string, unknown> = {},
  ) {
    super(message);
  }
}

export class PasswordRequired extends ApiError {}

/** Guest API client. Works identically over every transport. */
export class Api {
  private token = "";
  private password = "";
  private authing: Promise<SessionInfo> | null = null;
  session: SessionInfo | null = null;

  constructor(
    private getTransport: () => Promise<Transport>,
    private key: string,
  ) {}

  setPassword(pw: string) {
    this.password = pw;
  }

  async auth(): Promise<SessionInfo> {
    if (this.authing) return this.authing;
    this.authing = (async () => {
      const t = await this.getTransport();
      const res = await t.request({
        method: "POST",
        uri: "/api/auth",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key: this.key, password: this.password }),
      });
      const j = (await res.json().catch(() => ({}))) as Record<string, unknown>;
      if (res.status !== 200) {
        if (j.passwordRequired) throw new PasswordRequired(String(j.error ?? "Password required"), res.status, j);
        throw new ApiError(String(j.error ?? `Error ${res.status}`), res.status, j);
      }
      this.token = String(j.token);
      this.session = j.session as SessionInfo;
      return this.session;
    })();
    try {
      return await this.authing;
    } finally {
      this.authing = null;
    }
  }

  get bearer() {
    return this.token;
  }

  /**
   * Performs a request with the auth token. Expired tokens are renewed once;
   * idempotent requests are retried after connection loss.
   */
  async raw(
    method: string,
    uri: string,
    opts: {
      body?: Blob | Uint8Array | string | null;
      headers?: Record<string, string>;
      signal?: AbortSignal;
      onUploadProgress?: (n: number) => void;
      retry?: boolean;
      /** Send over this connection instead of the primary one. */
      transport?: Transport;
    } = {},
  ): Promise<TunnelResponse> {
    const retry = opts.retry ?? method === "GET";
    for (let attempt = 0; ; attempt++) {
      const t = opts.transport && !opts.transport.closed() ? opts.transport : await this.getTransport();
      try {
        const res = await t.request({
          method,
          uri,
          body: opts.body,
          signal: opts.signal,
          onUploadProgress: opts.onUploadProgress,
          headers: { ...(opts.headers ?? {}), Authorization: "Bearer " + this.token },
        });
        if (res.status === 401 && attempt === 0) {
          await res.text().catch(() => {});
          await this.auth();
          continue;
        }
        if (res.status === 429 && attempt < 30) {
          await res.text().catch(() => {});
          const after = Number(res.headers.get("retry-after") ?? "2");
          await sleep(Math.min(after, 10) * 1000, opts.signal);
          continue;
        }
        return res;
      } catch (e) {
        if ((e as Error).name === "AbortError" || !retry || attempt >= 5) throw e;
        await sleep(500 * (attempt + 1), opts.signal);
      }
    }
  }

  async json<T>(method: string, uri: string, body?: unknown): Promise<T> {
    const res = await this.raw(method, uri, {
      body: body === undefined ? null : JSON.stringify(body),
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
    });
    const j = (await res.json().catch(() => ({}))) as Record<string, unknown>;
    if (res.status >= 400) throw new ApiError(String(j.error ?? `Error ${res.status}`), res.status, j);
    return j as T;
  }

  list(path: string) {
    return this.json<{ path: string; entries: Entry[]; truncated: boolean }>("GET", "/api/list?path=" + encodeURIComponent(path));
  }
  search(path: string, q: string) {
    return this.json<{ results: Entry[]; partial: boolean }>("GET", `/api/search?path=${encodeURIComponent(path)}&q=${encodeURIComponent(q)}`);
  }
  mkdir(path: string) {
    return this.json<Entry>("POST", "/api/mkdir", { path });
  }
  remove(path: string) {
    return this.json<{ ok: boolean }>("POST", "/api/delete", { path });
  }
  refreshSession() {
    return this.json<SessionInfo>("GET", "/api/session").then((s) => (this.session = s));
  }

  downloadURI(e: Entry) {
    return `/api/download/${encodeURIComponent(e.name)}?path=${encodeURIComponent(e.path)}`;
  }
  zipURI(path: string, name: string) {
    return `/api/zip/${encodeURIComponent(name + ".zip")}?path=${encodeURIComponent(path)}`;
  }
}

export function sleep(ms: number, signal?: AbortSignal) {
  return new Promise<void>((res, rej) => {
    const t = setTimeout(res, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        rej(new DOMException("Aborted", "AbortError"));
      },
      { once: true },
    );
  });
}
