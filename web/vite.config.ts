import path from "path"
import { defineConfig } from "vitest/config"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"

// The platform's routes under /apps are /apps, /apps/new and /apps/{id}[/{tab}],
// where {id} is an app's UUID; a deployed app is /apps/{slug}/… and a slug is
// never a UUID. That shape difference is the whole disambiguation, and it is the
// same one the backend makes — it looks the first segment up as a slug and falls
// through to the SPA when there is no such app (api/proxy_handler.go).
const APP_UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

function isPlatformAppsRoute(url = "") {
  const segment = url.split("?")[0].replace(/^\/apps\/?/, "").split("/")[0]
  return segment === "" || segment === "new" || APP_UUID.test(segment)
}

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      // import.meta.dirname, not __dirname: Vite's `configLoader: 'native'`
      // loads this file as a real ES module, where __dirname does not exist.
      // It is planned to become the default in a future major.
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    proxy: {
      // ws: true on both entries, because an upgrade is not an ordinary
      // request — without it Vite answers the handshake itself and the socket
      // dies immediately. The platform streams logs over a WebSocket under
      // /api, and deployed apps run on them wholesale (Streamlit's
      // _stcore/stream, Next's HMR, anything with a live connection).
      "/api": {
        target: "http://localhost:8080",
        ws: true,
      },
      // Storage links (ADR §12a) are a server route on the platform origin,
      // navigations included: the backend signs in, checks the grant and
      // redirects, and there is no SPA page behind them.
      "/files": {
        target: "http://localhost:8080",
      },
      // Deployed-app assets and XHR go to the backend. Browser navigations
      // (Accept: text/html) are split: the platform's own pages must stay with
      // Vite so SPA routes survive a hard refresh with HMR intact, while a
      // deployed app has to reach the backend that serves it.
      //
      // The split only matters when the backend runs single-origin
      // (APPS_PORT == PORT), which puts deployed apps under /apps too — with a
      // separate apps gateway they were never on this port at all. Sending
      // every HTML navigation to index.html then renders the dashboard in place
      // of the app, which is what it did.
      "/apps": {
        target: "http://localhost:8080",
        ws: true,
        bypass: (req) =>
          req.headers.accept?.includes("text/html") && isPlatformAppsRoute(req.url)
            ? "/index.html"
            : undefined,
      },
    },
  },
  build: {
    outDir: "dist",
  },
  // The settings index's guard reads page sources with `fs` and asserts nothing
  // about the DOM, so it needs no jsdom and no setup file — `npm test` is the
  // whole apparatus.
  test: {
    // `.tsx` as well as `.ts`: a helper that lives beside JSX-carrying siblings
    // is still testable, and moving it out to make the extension match would put
    // one of the apps list's facets somewhere the other two are not.
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    environment: "node",
  },
})
