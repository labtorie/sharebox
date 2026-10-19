import { describe, expect, it } from "vitest";
import { formatBytes, formatSpeed } from "./format";

describe("formatBytes", () => {
  it("never shows fractional bytes", () => {
    expect(formatBytes(123.456789)).toBe("123 B");
    expect(formatSpeed(0.4)).toBe("");
    expect(formatSpeed(512.7)).toBe("513 B/s");
    expect(formatBytes(1023.6)).toBe("1.0 KB");
  });
  it("uses one decimal below 10 and whole numbers above", () => {
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatSpeed(1.83 * 1024 * 1024)).toBe("1.8 MB/s");
    expect(formatSpeed(23.6 * 1024 * 1024)).toBe("24 MB/s");
  });
});
