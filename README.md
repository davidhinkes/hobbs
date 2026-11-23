# hobbs

An MCP server that makes official US aviation data available to AI agents: NOAA Aviation Weather Center advisories (SIGMET, AIRMET, international SIGMET, CWA) and NOTAMs from the FAA NOTAM Management Service. Every response carries its sources, retrieval times and any problems with the data pull, so an agent can tell a genuine "none" from a failure.

**Provided as is, with no warranty or guarantee.** It is a best-effort copy of data from the sources listed in each response and may be incomplete, delayed or wrong. It is not FAA or NWS data as published and is not endorsed by either. It is advisory only and not an official briefing: consult official sources before flight.

The server only makes data available (uniform, consistent, available). It does not decide what is relevant to a flight; see `docs/DESIGN.md` §0.

## Tools

| Tool | What it does |
|---|---|
| `get_notams` | NOTAMs from the FAA NMS-API, filtered by the FAA's own filters (location, area, time window, ...) |
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
HOBBS_NMS_CLIENT_ID=... HOBBS_NMS_CLIENT_SECRET=... ./hobbs -nms staging

# hosted over Streamable HTTP (requires at least one bearer token):
HOBBS_TOKENS=<long-random-token> HOBBS_NMS_CLIENT_ID=... HOBBS_NMS_CLIENT_SECRET=... ./hobbs -http :8080 -nms staging
```

| Flag | Default | Meaning |
|---|---|---|
| `-http` | (stdio) | Listen address for Streamable HTTP, e.g. `:8080`; on Railway use `-http :$PORT` |
| `-airworthy` | `false` | Declares responses usable for flight planning and navigation. By default every response says `"airworthy": false`, carries a `NOT_AIRWORTHY` warning (so nothing reads as complete), and the tool descriptions say so. Refused with `-nms staging` |
| `-nms` | `off` | NOTAM source: `off`, `staging` or `production` |
| `-rate` | `5` | HTTP only: global request rate limit, requests per second (bursts up to twice that); an error without `-http` |

| Secret (environment only) | Needed for |
|---|---|
| `HOBBS_TOKENS` | `-http`: comma-separated bearer tokens, each at least 16 characters |
| `HOBBS_NMS_CLIENT_ID`, `HOBBS_NMS_CLIENT_SECRET` | `-nms staging` or `-nms production` |

Example MCP client configuration (stdio):

```json
{ "mcpServers": { "hobbs": { "command": "/path/to/hobbs", "args": ["-nms", "staging"], "env": { "HOBBS_NMS_CLIENT_ID": "...", "HOBBS_NMS_CLIENT_SECRET": "..." } } } }
```

## Develop

```sh
go test ./...                                     # offline, uses captured fixtures in testdata/
go test -tags live ./...                          # also against the live sources (NOTAM tests need the NMS_* secrets)
go test -tags tfrlive ./internal/source/tfr       # the switched-off TFR adapter against tfr.faa.gov
```

The TFR adapter (`internal/source/tfr`) is kept but turned off: nothing calls it, a client refuses to fetch unless `Enabled` is set, and its live test is only built with `-tags tfrlive`.

Design: `docs/DESIGN.md`. What the upstream services actually do: `docs/SPIKE.md`.
