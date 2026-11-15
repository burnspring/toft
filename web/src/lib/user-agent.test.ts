import { describe, expect, it } from "vitest";
import { describeDevice } from "./user-agent";

const label = (ua: string) => describeDevice(ua)?.label;

describe("describeDevice", () => {
  it("names the browsers people actually sign in from", () => {
    expect(
      label(
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
      ),
    ).toBe("Chrome 152 on Windows");

    expect(
      label(
        "Mozilla/5.0 (iPhone; CPU iPhone OS 26_6_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/151.0.7922.112 Mobile/15E148 Safari/604.1",
      ),
    ).toBe("Chrome 151 on iPhone");

    expect(
      label(
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15",
      ),
    ).toBe("Safari 18 on macOS");

    expect(
      label("Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"),
    ).toBe("Firefox 128 on Linux");
  });

  it("does not let a Chromium fork answer as Chrome", () => {
    expect(
      label(
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
      ),
    ).toBe("Edge 152 on Windows");
  });

  it("keeps a headless browser distinguishable, because that is the point", () => {
    expect(
      label(
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/151.0.0.0 Safari/537.36",
      ),
    ).toBe("Headless Chrome 151 on Linux");
  });

  it("leaves a client that already names itself alone", () => {
    expect(label("toft/1.4.0 (linux; amd64)")).toBe("toft/1.4.0");
    expect(label("curl/8.5.0")).toBe("curl/8.5.0");
  });

  it("gives up out loud rather than inventing a device", () => {
    const d = describeDevice("SomeInternalRobot/9");
    expect(d?.known).toBe(false);
    expect(d?.label).toBe("SomeInternalRobot/9");
  });

  it("is absent when there is nothing to describe", () => {
    expect(describeDevice(undefined)).toBeUndefined();
    expect(describeDevice("")).toBeUndefined();
    expect(describeDevice("   ")).toBeUndefined();
  });

  it("always keeps the string it came from", () => {
    const ua = "Mozilla/5.0 (Windows NT 10.0) Chrome/152.0.0.0 Safari/537.36";
    expect(describeDevice(ua)?.raw).toBe(ua);
  });
});
