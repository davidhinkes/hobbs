# hobbs

An MCP server that makes official US aviation data available to AI agents: NOAA Aviation Weather Center advisories (SIGMET, AIRMET, international SIGMET, CWA) and NOTAMs from the FAA NOTAM Management Service. Every response carries its sources, retrieval times and any problems with the data pull, so an agent can tell a genuine "none" from a failure.

**Provided as is, with no warranty or guarantee.** It is a best-effort copy of data from the sources listed in each response and may be incomplete, delayed or wrong. It is not FAA or NWS data as published and is not endorsed by either. It is advisory only and not an official briefing: consult official sources before flight.

The server only makes data available (uniform, consistent, available). It does not decide what is relevant to a flight; see `docs/DESIGN.md` §0.

## Tools

| Tool | What it does |
|---|---|
| `get_notams` | NOTAMs from the FAA NMS-API, filtered by the FAA's own filters: a list of airports and en-route centers (with each airport's center added), a point and radius, a time window, ... |
| `get_weather_advisories` | The full current SIGMET / AIRMET / international SIGMET / CWA sets |
| `source_status` | Live health of each source |

There is no TFR tool yet: no official, usable TFR source exists (`docs/SPIKE.md`). TFRs appear in `get_notams` as FDC NOTAMs (text and dates, not outlines).

## Run it

Configuration is by command-line flag (`hobbs -h`). Secrets are the only exception: they are read from environment variables, so they never show up in process listings or shell history.

```sh
go build -o hobbs ./cmd/hobbs

# stdio, for a local MCP client (weather only):
./hobbs

# with NOTAMs from the FAA NMS-API staging environment:
HOBBS_NMS_CLIENT_ID=... HOBBS_NMS_CLIENT_SECRET=... ./hobbs -nms-url https://api-staging.cgifederal-aim.com

# hosted over Streamable HTTP (the endpoint is open; there is no authentication):
HOBBS_NMS_CLIENT_ID=... HOBBS_NMS_CLIENT_SECRET=... ./hobbs -http :8080 -nms-url https://api-staging.cgifederal-aim.com
```

| Flag | Default | Meaning |
|---|---|---|
| `-http` | (stdio) | Listen address for Streamable HTTP, e.g. `:8080`; on Railway use `-http :$PORT` |
| `-airworthy` | `false` | Declares responses usable for flight planning and navigation. By default every response says `"airworthy": false`, carries a `NOT_AIRWORTHY` warning (so nothing reads as complete), and the tool descriptions say so. Accepted only without `-nms-url` or with the FAA's production NMS-API URL (`https://api-nms.aim.faa.gov`); any other server, staging included, counts as test data |
| `-nms-url` | (NOTAMs off) | NMS-API server for NOTAMs, scheme and host only. Staging is `https://api-staging.cgifederal-aim.com` |
| `-rate` | `5` | HTTP only: global request rate limit, requests per second (bursts up to twice that); an error without `-http` |

| Secret (environment only) | Needed for |
|---|---|
| `HOBBS_NMS_CLIENT_ID`, `HOBBS_NMS_CLIENT_SECRET` | `-nms-url` |

Example MCP client configuration (stdio):

```json
{ "mcpServers": { "hobbs": { "command": "/path/to/hobbs", "args": ["-nms-url", "https://api-staging.cgifederal-aim.com"], "env": { "HOBBS_NMS_CLIENT_ID": "...", "HOBBS_NMS_CLIENT_SECRET": "..." } } } }
```

## Host it (Railway)

The repo has a `Dockerfile`. On Railway, deploy it from GitHub and set:

- **Start command:** `/usr/local/bin/hobbs -http :8080 -nms-url https://api-staging.cgifederal-aim.com` (drop `-nms-url` for weather only)
- **Public domain** targeting port `8080`; the MCP endpoint is `https://<domain>/mcp`
- **Health check:** `/healthz`
- **Variables (secrets):** `HOBBS_NMS_CLIENT_ID`, `HOBBS_NMS_CLIENT_SECRET` (only with `-nms-url`)

The endpoint is open: anyone with the URL can use it, and the global rate limit (`-rate`) is the only guard on upstream quotas.

The repo's `.mcp.json` registers the hosted endpoint (`https://hobbs.up.railway.app/mcp`) as the `hobbs` MCP server, so Claude Code sessions on this repo load its tools at startup and can test the production service end to end. In a cloud session the environment's network policy must allow `hobbs.up.railway.app`.

## Develop

```sh
go test ./...                                     # offline, uses captured fixtures in testdata/
go test -tags live ./...                          # also against the live sources (NOTAM tests need the NMS_* secrets)
go test -tags tfrlive ./internal/source/tfr       # the switched-off TFR adapter against tfr.faa.gov
```

The TFR adapter (`internal/source/tfr`) is kept but turned off: nothing calls it, a client refuses to fetch unless `Enabled` is set, and its live test is only built with `-tags tfrlive`.

Design: `docs/DESIGN.md`. What the upstream services actually do: `docs/SPIKE.md`.
