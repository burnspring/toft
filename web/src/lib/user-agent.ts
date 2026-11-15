/**
 * A User-Agent string, as a device somebody would recognise.
 *
 * `/me/security` is the one page a person opens to answer *did somebody else
 * sign in as me*, and the column that answers it printed
 * `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like
 * Gecko) Chrome/152.0.0.0 Safari/537.36` — twenty-eight times, near-identically,
 * with the one iPhone row and the two loopback rows invisible in the middle of
 * it. Naming a thing by how it is built rather than by what a person recognises
 * is the failure mode; here it also hid the anomaly the page exists to surface.
 *
 * This is deliberately not a UA-parsing library. It recognises the handful of
 * families a person signs in from and gives up honestly on everything else,
 * because the raw string stays available and a wrong confident answer on a
 * security page is worse than no answer.
 */

/** Browsers, in the order they have to be tested. */
const BROWSERS: { re: RegExp; name: string }[] = [
  // Chromium forks announce themselves as Chrome too, so they go first.
  { re: /\bEdg(?:e|A|iOS)?\/(\d+)/, name: "Edge" },
  { re: /\bOPR\/(\d+)/, name: "Opera" },
  { re: /\bSamsungBrowser\/(\d+)/, name: "Samsung Internet" },
  { re: /\bHeadlessChrome\/(\d+)/, name: "Headless Chrome" },
  // Chrome and Firefox on iOS are Safari underneath and say so; their own
  // markers come first for the same reason.
  { re: /\bCriOS\/(\d+)/, name: "Chrome" },
  { re: /\bFxiOS\/(\d+)/, name: "Firefox" },
  { re: /\bChrome\/(\d+)/, name: "Chrome" },
  { re: /\bFirefox\/(\d+)/, name: "Firefox" },
  { re: /\bVersion\/(\d+).*\bSafari\//, name: "Safari" },
];

/** Platforms, coarse on purpose: the build number of a kernel is not a device. */
const PLATFORMS: { re: RegExp; name: string }[] = [
  { re: /\biPhone\b/, name: "iPhone" },
  { re: /\biPad\b/, name: "iPad" },
  { re: /\bAndroid\b/, name: "Android" },
  { re: /\bCrOS\b/, name: "ChromeOS" },
  { re: /\bWindows NT\b/, name: "Windows" },
  { re: /\bMac OS X\b|\bMacintosh\b/, name: "macOS" },
  { re: /\bLinux\b|\bX11\b/, name: "Linux" },
];

/**
 * A non-browser client that already names itself readably — `toft/1.4.0`,
 * `curl/8.5.0`, `python-requests/2.31`. These are shown as they arrived: they
 * are short, they are the product's own vocabulary, and rewriting them would
 * lose the version that makes them useful.
 */
const SELF_NAMING = /^(?:toft|toft[\w-]*|curl|wget|httpie|python-requests|okhttp|go-http-client|postmanruntime)\//i;

export interface Device {
  /** What to show. Never empty. */
  label: string;
  /** The string it came from, for the row's title and its expansion. */
  raw: string;
  /** Whether anything was actually recognised, so a caller can style a guess. */
  known: boolean;
}

export function describeDevice(userAgent: string | undefined | null): Device | undefined {
  const raw = (userAgent ?? "").trim();
  if (!raw) return undefined;

  if (SELF_NAMING.test(raw)) {
    // Already a name and a version, and nothing here improves on it.
    return { label: raw.split(/\s+/)[0], raw, known: true };
  }

  const browser = BROWSERS.find((b) => b.re.test(raw));
  const platform = PLATFORMS.find((p) => p.re.test(raw));

  if (!browser && !platform) {
    // Give up out loud rather than inventing a device. The first token is the
    // closest thing to a name an unknown agent has.
    return { label: raw.split(/\s+/)[0].slice(0, 40), raw, known: false };
  }

  const version = browser ? (raw.match(browser.re)?.[1] ?? "") : "";
  const name = browser ? [browser.name, version].filter(Boolean).join(" ") : "";

  if (name && platform) return { label: `${name} on ${platform.name}`, raw, known: true };
  return { label: name || platform!.name, raw, known: true };
}
