# Toft

**Toft is where your organization's own software lives.**

Web apps, APIs, dashboards, reports and scheduled work — anything you build for
yourselves, whether a person wrote it or an agent did. Each one gets its own web
address, your normal sign-in in front of it, permission to reach the data it
needs, and someone responsible for it. Paste a GitHub repository URL, or push
the working tree from your laptop: Toft builds it, deploys it and serves it. No
Dockerfile, no Kubernetes knowledge, no ticket.

It is one Go binary and a Postgres database, on your own machines. It detects
Next.js, Node, Go, Flask, FastAPI, Streamlit, Shiny, Plumber, Quarto, generic
Python and R, and static sites from what is in the repository.

- **Manuals:** [docs.gettoft.com](https://docs.gettoft.com) — for people who
  deploy apps, for administrators, and for the CLI and the app SDKs.
- **Site:** [gettoft.com](https://gettoft.com)

## Run it from source

You need Docker with Compose. From a clone of this repository:

```bash
cd deploy/compose
cp .env.example .env    # fill in the secrets it lists; each says how to make one
docker compose -f compose.yml -f compose.build.yml up -d --build
```

That builds the image from this checkout (the web app, then the binary, then
the language runtimes — the first build takes a while) and starts it beside
Postgres. Toft listens on `127.0.0.1:8080` and serves apps on `127.0.0.1:8081`.
Sign in as `admin`; the first password is printed once, in a `FIRST BOOT`
banner in `docker compose logs toft`, and the first sign-in replaces it.

## Build it yourself

```bash
cd web && npm ci && npm run build && cd ..
rm -rf internal/spa/dist && cp -r web/dist internal/spa/dist
go build -o toft .
```

The binary embeds the web app, so it is built second. The image is
`docker build -t <registry>/toft:<version> .`, for both `linux/amd64` and
`linux/arm64`.

## Kubernetes

The Helm chart is `deploy/charts/toft`. No image is published: build one, push
it to a registry the cluster can pull from, and set `image.repository`.
`deploy/charts/toft/values-aks.yaml` is a worked example for Azure.

## The CLI and the SDKs

`toft`, the command-line client, is its own Go module in `cli/`
(`cd cli && go build -o toft .`). The app SDKs for Python, Node, Go and R are in
`sdk/`; the binary embeds them and installs the right one into every app it
builds.

## Tests

```bash
go test ./...            # the unit tests; those that need Postgres skip without it
(cd cli && go test ./...)
(cd web && npm ci && npm test)
```

## Reporting problems

Bugs and feature requests go to this repository's issues, and questions to its
Discussions. **A security vulnerability goes by email to
security@gettoft.com, never to a public issue** — see `SECURITY.md`.

## License

Apache License 2.0 — see `LICENSE` and `NOTICE`. "Toft" is a name of
Burnspring's; a fork is welcome under a name of its own.
