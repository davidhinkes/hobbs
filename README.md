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

```sh
go build -o hobbs ./cmd/hobbs

# stdio, for a local MCP client (weather works with no configuration):
./hobbs

# with NOTAMs (FAA NMS-API credentials, from the environment only):
NMS_HOST=https://api-staging.cgifederal-aim.com NMS_ENV=staging \
NMS_CLIENT_ID=... NMS_CLIENT_SECRET=... ./hobbs

# hosted over Streamable HTTP (requires at least one bearer token):
HOBBS_TOKENS=<long-random-token> ./hobbs -http :8080
```

Set `NMS_ENV=production` only for the operational NMS service; anything else is treated as a non-production environment and every NOTAM response says so.

Example MCP client configuration (stdio):

```json
{ "mcpServers": { "hobbs": { "command": "/path/to/hobbs", "env": { "NMS_HOST": "...", "NMS_CLIENT_ID": "...", "NMS_CLIENT_SECRET": "..." } } } }
```

## Develop

```sh
go test ./...                 # offline, uses captured fixtures in testdata/
HOBBS_LIVE=1 go test ./...    # also against the live sources (NOTAM tests run only if NMS_* are set)
```

The TFR adapter (`internal/source/tfr`) is kept but turned off: nothing calls it, a client refuses to fetch unless `Enabled` is set, and its live test runs only with `HOBBS_ENABLE_TFR=1`.

Design: `docs/DESIGN.md`. What the upstream services actually do: `docs/SPIKE.md`.
