// Sharebox service worker: turns a stream of chunks produced by the page
// (received over WebRTC) into a real browser download, so files of any size
// are written to disk without being held in memory.
//
// Protocol: the page posts {type:"register", id, name, size, mime} with a
// MessagePort, then navigates a hidden iframe to /__sbx_dl/<id>/<name>.
// The response body pulls chunks through the port: we send "pull", the page
// answers {chunk} | {end} | {error}.

const downloads = new Map();

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("message", (e) => {
  const d = e.data || {};
  if (d.type === "register" && e.ports[0]) {
    downloads.set(d.id, { ...d, port: e.ports[0] });
    // Confirm, so the page only starts the download request once we know
    // about it (message and fetch events are not ordered).
    e.ports[0].postMessage("registered");
  }
  // "ping" messages from the page keep this worker alive during long downloads.
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (!url.pathname.startsWith("/__sbx_dl/")) return;
  const id = url.pathname.split("/")[2];
  const dl = downloads.get(id);
  if (!dl) {
    e.respondWith(new Response("Download expired", { status: 404 }));
    return;
  }
  downloads.delete(id);
  const port = dl.port;
  port.postMessage("started");
  let pending = null;
  port.onmessage = (m) => {
    const p = pending;
    pending = null;
    if (p) p(m.data);
  };
  const stream = new ReadableStream({
    pull(controller) {
      return new Promise((resolve) => {
        pending = (msg) => {
          if (msg.chunk) controller.enqueue(new Uint8Array(msg.chunk));
          else if (msg.end) controller.close();
          else controller.error(new Error(msg.error || "download failed"));
          resolve();
        };
        port.postMessage("pull");
      });
    },
    cancel() {
      port.postMessage("cancel");
    },
  }, { highWaterMark: 0 });
  const enc = encodeURIComponent(dl.name).replace(/['()]/g, escape);
  const ascii = dl.name.replace(/[^\x20-\x7e]|["\\;]/g, "_");
  const headers = {
    "Content-Type": "application/octet-stream",
    "Content-Disposition": `attachment; filename="${ascii}"; filename*=UTF-8''${enc}`,
    "X-Content-Type-Options": "nosniff",
    "Content-Security-Policy": "sandbox",
  };
  if (dl.size >= 0) headers["Content-Length"] = String(dl.size);
  e.respondWith(new Response(stream, { headers }));
});
