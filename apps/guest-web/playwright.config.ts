import { defineConfig } from "@playwright/test";

// End-to-end tests drive real Google Chrome against the real coordination
// server, TURN relay and headless host (bin/ built by scripts/build.sh).
export default defineConfig({
  testDir: "e2e",
  timeout: 90_000,
  workers: 1,
  reporter: [["list"]],
  use: {
    channel: "chrome",
    headless: true,
    acceptDownloads: true,
  },
});
