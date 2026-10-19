// Interop test: TypeScript tunnel client against the Go host implementation.
import { spawn, type ChildProcess } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { TunnelClient } from "./tunnel";
import { Api } from "./api";
import { Transfers } from "./transfers";
import type { Transport } from "./transport";

let proc: ChildProcess;
let port = 0;
let key = "";
let dir = "";

beforeAll(async () => {
  proc = spawn(process.env.SBX_INTEROP_BIN ?? "go", process.env.SBX_INTEROP_BIN ? [] : ["run", "./tests/interop"], { cwd: fileURLToPath(new URL("../../../..", import.meta.url)), stdio: ["ignore", "pipe", "inherit"] });
  await new Promise<void>((resolve, reject) => {
    let buf = "";
    proc.stdout!.on("data", (d) => {
      buf += d;
      const m = /READY (\d+) (\S+) (.+)\n/.exec(buf);
      if (m) {
        [port, key, dir] = [Number(m[1]), m[2], m[3]];
        resolve();
      }
    });
    proc.on("exit", (c) => reject(new Error("interop server exited " + c)));
    proc.on("error", reject);
  });
}, 120_000);

afterAll(() => proc?.kill());

async function connect(): Promise<{ client: TunnelClient; transport: Transport }> {
  const ws = new WebSocket(`ws://127.0.0.1:${port}/`);
  ws.binaryType = "arraybuffer";
  await new Promise((res, rej) => ((ws.onopen = res), (ws.onerror = rej)));
  const client = new TunnelClient({ send: (b) => ws.send(b), get bufferedAmount() { return ws.bufferedAmount; }, close: () => ws.close() });
  ws.onmessage = (e) => client.deliver(e.data as ArrayBuffer);
  ws.onclose = () => client.close();
  const transport: Transport = { kind: "p2p", native: false, closed: () => client.isClosed, request: (r) => client.request(r) };
  return { client, transport };
}

describe("TS tunnel ⇄ Go host", () => {
  it("authenticates, lists and downloads with exact bytes", async () => {
    const { transport } = await connect();
    const api = new Api(async () => transport, key);
    const s = await api.auth();
    expect(s.permissions.download).toBe(true);
    const list = await api.list("");
    expect(list.entries.map((e) => e.name).sort()).toEqual(["big.bin", "hello.txt"]);

    const res = await api.raw("GET", "/api/download/big.bin?path=big.bin");
    expect(res.status).toBe(200);
    const r = res.body.getReader();
    const want = readFileSync(join(dir, "big.bin"));
    let off = 0;
    for (;;) {
      const { done, value } = await r.read();
      if (done) break;
      expect(Buffer.from(value).equals(want.subarray(off, off + value.length))).toBe(true);
      off += value.length;
    }
    expect(off).toBe(want.length); // larger than the flow-control window
  });

  it("handles Range requests and concurrent streams", async () => {
    const { transport } = await connect();
    const api = new Api(async () => transport, key);
    await api.auth();
    const want = readFileSync(join(dir, "big.bin"));
    const results = await Promise.all(
      [0, 1, 2, 3, 4, 5].map(async (i) => {
        const start = i * 100_000;
        const res = await api.raw("GET", "/api/download/big.bin?path=big.bin", { headers: { Range: `bytes=${start}-${start + 99_999}` } });
        const buf = new Uint8Array(await new Response(res.body).arrayBuffer());
        return res.status === 206 && Buffer.from(buf).equals(want.subarray(start, start + 100_000));
      }),
    );
    expect(results.every(Boolean)).toBe(true);
  });

  it("uploads in verified chunks through the transfer manager", async () => {
    const { transport } = await connect();
    const api = new Api(async () => transport, key);
    await api.auth();
    const data = new Uint8Array(9 * 1024 * 1024 + 17).map((_, i) => (i * 13) & 0xff);
    const tr = new Transfers(api, async () => transport);
    const done = new Promise<void>((res) => (tr.onUploaded = () => res()));
    tr.upload(new File([data], "from-ts.bin"), "");
    await done;
    tr.dispose();
    const disk = readFileSync(join(dir, "from-ts.bin"));
    expect(disk.length).toBe(data.length);
    expect(Buffer.from(data).equals(disk)).toBe(true);
  }, 60_000);

  it("cancels a download mid-stream", async () => {
    const { client, transport } = await connect();
    const api = new Api(async () => transport, key);
    await api.auth();
    const ctl = new AbortController();
    const res = await api.raw("GET", "/api/download/big.bin?path=big.bin", { signal: ctl.signal });
    const r = res.body.getReader();
    await r.read();
    ctl.abort();
    await expect(r.read()).rejects.toBeTruthy();
    // The tunnel remains usable for other requests.
    expect((await api.list("")).entries.length).toBeGreaterThan(0);
    expect(client.isClosed).toBe(false);
  });

  it("rejects a wrong key", async () => {
    const { transport } = await connect();
    const api = new Api(async () => transport, "x".repeat(43));
    await expect(api.auth()).rejects.toThrow();
  });
});
