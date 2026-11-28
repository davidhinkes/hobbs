# hobbs

An MCP server that makes official US aviation data available to AI agents: NOTAMs from the FAA NOTAM Management Service, and METARs, TAFs and weather advisories (SIGMET, AIRMET, international SIGMET, CWA) from the NOAA Aviation Weather Center, for a flight's airports in one call. Every data response carries its sources, retrieval times and any problems with the data pull, so an agent can tell a genuine "none" from a failure.

**Provided as is, with no warranty or guarantee.** It is a best-effort copy of data from the sources listed in each response and may be incomplete, delayed or wrong. It is not FAA or NWS data as published and is not endorsed by either. It is advisory only and not an official briefing: consult official sources before flight.

The server only makes data available (uniform, consistent, available). It does not decide what is relevant to a flight; see `docs/DESIGN.md` §2.

**Hosted:** `https://hobbs.up.railway.app/mcp` (Streamable HTTP, open, no authentication). It serves NOTAMs from the FAA's **staging** environment, so every data response says `"airworthy": false`.

## Tools

| Tool | What it does |
|---|---|
| `get_flight_briefing_data` | For a flight's airports (4-character ICAO identifiers, e.g. `KSMO`) and any en-route centers it also crosses: NOTAMs from the FAA NMS-API (the en-route center the FAA lists on the airports' NOTAMs is added), the latest METARs and TAFs from AWC, and the full current SIGMET / AIRMET / international SIGMET / CWA sets, each in its own sublist under one provenance envelope. `include` chooses the sublists (default all); `translation` shows the parameters sent to each source for the airports and centers given |
| `source_status` | Live health of each source (the four advisory feeds, METAR, TAF, NOTAMs) |
| `get_briefing_guide` | A short guide for an agent briefing a general aviation pilot with this data: what to ask, what to fetch, how to keep the briefing short, what never to cut, and to take data that changes (weather, NOTAMs, restrictions) only from hobbs, never from other sites or memory. It is advice on method written by this project, not data, and the agent is told to call it first (`docs/DESIGN.md` §5.2) |

There is no TFR tool yet: no official, usable TFR source exists (`docs/DESIGN.md` §5.5). TFRs appear in `get_flight_briefing_data`'s NOTAMs as FDC NOTAMs (text and dates, not outlines).

For a flight, list the airports (departure, destination, alternates). The en-route center the FAA lists on their NOTAMs is added; centers in between are not inferred, so list any other center the flight crosses:

```json
{ "name": "get_flight_briefing_data", "arguments": { "airports": ["KSMO", "KPSP", "KTRM"], "centers": ["ZAB"] } }
```

There is no time window, route, area or radius input, and no `limit`: the server returns everything the sources return for those places, and leaves relevance to the caller. Geometry is left out unless `include_geometry` is `true`. 3-letter FAA identifiers (`SMO`) are refused with a message naming the ICAO form; the server never adds a `K` itself.

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
| `-http` | (stdio) | Listen address for Streamable HTTP, e.g. `:8080` |
| `-airworthy` | `false` | Declares responses usable for flight planning and navigation. By default every data response says `"airworthy": false`, carries a `NOT_AIRWORTHY` warning (so nothing reads as complete), and the tool descriptions say so. Accepted only without `-nms-url` or with the FAA's production NMS-API URL (`https://api-nms.aim.faa.gov`); any other server, staging included, counts as test data |
| `-nms-url` | (NOTAMs off) | NMS-API server for NOTAMs, scheme and host only. Staging is `https://api-staging.cgifederal-aim.com` |
| `-rate` | `5` | HTTP only: global limit on incoming MCP requests, per second (bursts up to twice that); an error without `-http`. It does not cap NMS traffic: one briefing makes an NMS request per airport and center |
| `-nms-rate` | `1` | NMS-API requests per second, at most, with no burst (token, NOTAM and ping requests alike). Requests over it wait their turn rather than fail, so a 10-airport briefing takes 10 s or more. The FAA's NMS-API FAQ gives the test environment 1 per second |

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
go test -tags live ./...                          # also against the live sources (NOTAM tests need HOBBS_NMS_CLIENT_ID/SECRET)
go test -tags tfrlive ./internal/source/tfr       # the switched-off TFR adapter against tfr.faa.gov
```

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, the build, the offline tests, `staticcheck`, `deadcode` and a Docker build on every pull request and every push to `main`.

The TFR adapter (`internal/source/tfr`) is kept but turned off: nothing calls it, a client refuses to fetch unless `Enabled` is set, and its live test is only built with `-tags tfrlive`.

Design: `docs/DESIGN.md`: principles, product requirements and user flows, a high-level design with a diagram, the detailed design (including how the upstream services behave), open items, and the decisions log with alternatives considered. Working notes for Claude Code sessions: `CLAUDE.md`.
