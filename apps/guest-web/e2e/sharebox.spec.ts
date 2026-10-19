import { test, expect, type Page } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const repo = fileURLToPath(new URL("../../..", import.meta.url));
const bin = (n: string) => join(repo, "bin", n);
const PORT = 18080;
const TURN = 13478;
const server = `http://127.0.0.1:${PORT}`;
const procs: ChildProcess[] = [];
const sha = (b: Buffer) => createHash("sha256").update(b).digest("hex");

function startServer() {
  const p = spawn(bin("sharebox-server"), ["-dev", "-listen", `127.0.0.1:${PORT}`, "-public-url", server, "-turn-udp", `127.0.0.1:${TURN}`], { stdio: "ignore" });
  procs.push(p);
  return new Promise((r) => setTimeout(r, 700));
}

interface Host { proc: ChildProcess; link: string; dir: string }

function makeShare(): string {
  const dir = mkdtempSync(join(tmpdir(), "sbx-e2e-"));
  mkdirSync(join(dir, "Photos"));
  mkdirSync(join(dir, "Docs"));
  writeFileSync(join(dir, "readme.txt"), "Hello from Sharebox");
  writeFileSync(join(dir, "Docs", "notes.md"), "# Notes");
  const big = Buffer.alloc(24 * 1024 * 1024 + 99);
  for (let i = 0; i < big.length; i++) big[i] = (i * 17 + 3) & 255;
  writeFileSync(join(dir, "Photos", "video.mp4"), big);
  return dir;
}

function startHost(args: string[] = []): Promise<Host> {
  const dir = makeShare();
  const cfg = mkdtempSync(join(tmpdir(), "sbx-cfg-"));
  const proc = spawn(bin("sharebox-cli"), ["-server", server, "-folder", dir, "-no-portmap", "-delete", ...args], {
    env: { ...process.env, SHAREBOX_CONFIG_DIR: cfg },
    stdio: ["ignore", "pipe", "inherit"],
  });
  procs.push(proc);
  return new Promise((resolve, reject) => {
    let out = "";
    proc.stdout!.on("data", (d) => {
      out += d;
      const m = /(http:\/\/127\.0\.0\.1:\d+\/s\/\S+#k=\S+)/.exec(out);
      if (m) resolve({ proc, link: m[1], dir });
    });
    proc.on("exit", () => reject(new Error("host exited: " + out)));
  });
}

test.beforeAll(async () => {
  if (!existsSync(bin("sharebox-server"))) throw new Error("run scripts/build.sh first");
  await startServer();
});
test.afterAll(() => procs.forEach((p) => p.kill("SIGINT")));

async function open(page: Page, link: string, relay = false) {
  const u = new URL(link);
  if (relay) u.search = "?transport=relay";
  await page.goto(u.toString());
  await expect(page.getByText("readme.txt")).toBeVisible({ timeout: 20_000 });
  expect(page.url()).not.toContain("#k="); // secret removed from the address bar
}

test("peer-to-peer: browse, upload, download, zip, folders, search, delete", async ({ page }) => {
  const h = await startHost();
  await open(page, h.link);
  await expect(page.getByText("Peer-to-peer")).toBeVisible();

  // Browse into a folder and back via breadcrumbs.
  await page.getByRole("button", { name: "Photos" }).click();
  await expect(page.getByText("video.mp4")).toBeVisible();

  // Streamed download through the service worker; verify exact bytes.
  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "video.mp4" }).click()]);
  const saved = join(mkdtempSync(join(tmpdir(), "sbx-dl-")), "v.mp4");
  await dl.saveAs(saved);
  expect(sha(readFileSync(saved))).toBe(sha(readFileSync(join(h.dir, "Photos", "video.mp4"))));
  const viaSW = await page.evaluate(() => !!navigator.serviceWorker.controller);
  expect(viaSW).toBe(true);

  // Upload into the current folder.
  const up = Buffer.alloc(11 * 1024 * 1024 + 5);
  for (let i = 0; i < up.length; i++) up[i] = (i * 29) & 255;
  await page.locator('input[type=file][multiple]').setInputFiles({ name: "from-chrome.bin", mimeType: "application/octet-stream", buffer: up });
  await expect(page.getByText("Transfers complete")).toBeVisible({ timeout: 30_000 });
  expect(sha(readFileSync(join(h.dir, "Photos", "from-chrome.bin")))).toBe(sha(up));
  await expect(page.getByRole("button", { name: "from-chrome.bin" })).toBeVisible();

  // ZIP download of the whole share.
  await page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("button").first().click();
  await expect(page.getByText("readme.txt")).toBeVisible();
  const [zip] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: /Download all/ }).click()]);
  const zpath = join(mkdtempSync(join(tmpdir(), "sbx-zip-")), "a.zip");
  await zip.saveAs(zpath);
  const zb = readFileSync(zpath);
  expect(zb.subarray(0, 2).toString()).toBe("PK");
  expect(zb.length).toBeGreaterThan(35 * 1024 * 1024);

  // New folder.
  await page.getByRole("button", { name: /New folder/ }).click();
  await page.getByPlaceholder("Folder name").fill("Created in browser");
  await page.getByRole("button", { name: "Create" }).click();
  await expect(page.getByRole("button", { name: "Created in browser" })).toBeVisible();
  expect(existsSync(join(h.dir, "Created in browser"))).toBe(true);

  // Search across the share.
  await page.getByLabel("Search").fill("notes");
  await expect(page.getByText(/1 result/)).toBeVisible();
  await page.getByLabel("Clear search").click();

  // Delete (enabled for this host with -delete).
  await page.getByRole("row", { name: /readme\.txt/ }).hover();
  await page.getByRole("row", { name: /readme\.txt/ }).getByLabel("Delete").click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete", exact: true }).click();
  await expect(page.getByRole("button", { name: "readme.txt" })).toHaveCount(0);
  expect(existsSync(join(h.dir, "readme.txt"))).toBe(false);

  // Stopping the host ends access for the guest.
  h.proc.kill("SIGINT");
  await expect(page.getByText("This share has ended")).toBeVisible({ timeout: 15_000 });
});

test("relay: forced TURN path transfers correctly", async ({ page }) => {
  const h = await startHost();
  await open(page, h.link, true);
  await expect(page.getByText("Relayed")).toBeVisible();
  await page.getByRole("button", { name: "Photos" }).click();
  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "video.mp4" }).click()]);
  const saved = join(mkdtempSync(join(tmpdir(), "sbx-dl-")), "v.mp4");
  await dl.saveAs(saved);
  expect(sha(readFileSync(saved))).toBe(sha(readFileSync(join(h.dir, "Photos", "video.mp4"))));
  const up = Buffer.from("relay upload ".repeat(100000));
  await page.locator('input[type=file][multiple]').setInputFiles({ name: "relay.txt", mimeType: "text/plain", buffer: up });
  await expect(page.getByText("Transfers complete")).toBeVisible({ timeout: 30_000 });
  expect(sha(readFileSync(join(h.dir, "Photos", "relay.txt")))).toBe(sha(up));
});

test("password-protected share", async ({ page }) => {
  const h = await startHost(["-password", "open sesame"]);
  await page.goto(h.link);
  await expect(page.getByText("Password required")).toBeVisible({ timeout: 20_000 });
  await page.getByPlaceholder("Password").fill("wrong");
  await page.getByRole("button", { name: "Unlock" }).click();
  await expect(page.getByText("Wrong password")).toBeVisible();
  await page.getByPlaceholder("Password").fill("open sesame");
  await page.getByRole("button", { name: "Unlock" }).click();
  await expect(page.getByText("readme.txt")).toBeVisible();
});

test("invalid links show clear errors", async ({ page }) => {
  const h = await startHost();
  await page.goto(h.link.replace(/#k=.*/, "#k=" + "A".repeat(43)));
  await expect(page.getByText("This link is no longer valid")).toBeVisible({ timeout: 15_000 });
  await page.goto(`${server}/s/doesnotexist2345678#k=${"B".repeat(43)}`);
  await expect(page.getByText("This share doesn't exist")).toBeVisible({ timeout: 15_000 });
  // A fresh tab has no stored key for this share.
  const fresh = await page.context().newPage();
  await fresh.goto(h.link.replace(/#k=.*/, ""));
  await expect(fresh.getByText("This link is incomplete")).toBeVisible();
});
