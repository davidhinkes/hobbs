# Hobbs: aviation advisories MCP server — design

Status: draft. Evidence for source behavior is in `docs/SPIKE.md`. §0 governs everything else in this document: when a later section appears to conflict with it, §0 wins.

## 0. Mantra and boundary

> **The MCP layer makes the FAA's data available to the agent. Uniform, consistent, and available.**

- **Uniform:** every source comes out in the same shapes (one envelope, one NOTAM model, one advisory model, one altitude type), so an agent does not learn a new format per source.
- **Consistent:** the same inputs give the same kind of output, with stable ordering, stable identifiers and the same behavior on errors, gaps and odd data.
- **Available:** the data is reachable when the sources are, and when it is not (or is stale, partial or from a test environment) the response says so, instead of failing quietly.

This layer is the foundation. The opinionated parts of the application (what matters for a flight, what to do about it) are built **on top of it, in separate layers**, by people and agents who choose their own logic. They are deliberately not here.

### What this layer may do

Low-risk value-adds that make the data easier to consume without deciding anything for the user:

- Simplify and normalize: consistent field names, timestamps in UTC, coordinates in decimal degrees, one altitude representation, unit conversions, merging a source's multiple text renderings under one record.
- Interpret reasonably where a source is ambiguous or inconsistent (e.g. reading a limit stated as `6000FT AMSL`), **as long as** the original value is kept alongside, the interpretation is documented, and anything we could not interpret is flagged and not guessed.
- Forward the filters the official API supports, and say that the source applied them.
- Report on the data itself: where it came from, how old it is, what went wrong getting it.

### What this layer must not do

Anything where there is a decent chance we would miss something, or where logic makes a decision for the user implicitly:

- Decide what is relevant, what applies to a route, a time, an altitude or an aircraft, or what is active now.
- Rank, score, prioritize, group by importance, summarize or advise.
- Remove, merge or hide records on the strength of our own judgment (including de-duplicating or "superseding" NOTAMs).
- Fill gaps with assumptions that change what the data says.

### How to apply it

Before adding any behavior to this layer, ask:

1. **Does it change which records come back, or what a field means, in a way the caller cannot see or undo?** If yes, it does not belong here.
2. **If our logic is wrong, does the failure show or hide?** Showing (extra data, a flag, a visible parse issue) is acceptable. Hiding (a missing record, a silently changed meaning) is not.
3. **Is the original preserved next to anything we interpreted?** If not, it must be.
4. **Is it a decision the user or their agent would want to make themselves?** If yes, it belongs in a higher layer.

When in doubt, return the data as published, flag the doubt, and leave the decision to the layer above.

## 1. Provenance and honesty about data quality

**Every tool response, with no exceptions, states which official sources it used, when each was retrieved, and every problem with the data pull that could affect the result.** This is a hard requirement, not a feature. Users are agents, and agents read absence of evidence as evidence of absence, so the server must make that mistake impossible to make by accident.

Rules that follow:

1. **Official sources only.** Every upstream is an FAA/NOAA/ANSP system. No third-party aggregators. The source registry records, per upstream, the authority, URL, and whether the endpoint is *documented* or *undocumented* (e.g. `tfr.faa.gov/tfrapi/*`); undocumented status is itself reported to callers.
2. **No response without provenance.** Handlers cannot return bare data. They return through a single envelope constructor, so a response without provenance does not compile, and a test validates every tool's output against the schema.
3. **Never say "none" unless it is true.** An empty result is only returned as a clean "nothing found" when `completeness.status == "complete"`. Otherwise the empty result is accompanied by the issues that make it untrustworthy.
4. **Report what was not checked**, not only what was. Sources that were skipped, disabled, or failed appear in the response.
5. **Degrade, don't hide.** Stale cache, partial parses and missing geometry are returned and flagged. They are never dropped or silently patched.
6. **Raw official text is always carried alongside parsed fields**, in a separate field, treated as untrusted data (see §6).

### Response envelope

```jsonc
{
  "data": { /* tool-specific results */ },
  "evaluated_at": "2026-11-22T17:30:00Z",        // the instant the answer is "as of"
  "provenance": {
    "sources": [{
      "id": "awc.gairmet",
      "authority": "NOAA/NWS Aviation Weather Center",
      "url": "https://aviationweather.gov/api/data/gairmet?format=json",
      "documentation": "documented",              // documented | undocumented
      "retrieved_at": "2026-11-22T17:29:12Z",     // when WE fetched it
      "upstream_timestamp": "2026-11-22T17:25:00Z",// newest issue/receipt time in the data
      "age_seconds": 48,
      "served_from": "live",                      // live | cache | stale_cache
      "http_status": 200,
      "etag": "W/\"7d5d-...\"",
      "content_sha256": "…",
      "records_in_feed": 34,
      "records_returned": 3
    }],
    "not_consulted": [{ "id": "faa.nms.notam", "reason": "not_enabled" }]
  },
  "completeness": {
    "status": "incomplete",                       // complete | degraded | incomplete (any not_consulted entry => incomplete)
    "issues": [{
      "code": "GEOMETRY_UNAVAILABLE",
      "severity": "warning",                      // info | warning | critical
      "source": "faa.tfr",
      "message": "12 TFRs are text-only and cannot be evaluated spatially.",
      "affects": ["check_route"],
      "refs": ["FDC 0/0367"]
    }]
  },
  "disclaimer": "Provided AS IS, no warranty. Best-effort copy; not FAA/NWS data as published; not endorsed. Advisory only."
}
```

`completeness.status` is derived by rule, not by hand: `incomplete` if any source needed for the query failed or was not consulted, or any `critical` issue exists; `degraded` if any `warning` exists (stale data, partial parse, missing geometry); otherwise `complete`.

### Issue codes (initial set)

| Code | Meaning |
|---|---|
| `SOURCE_UNAVAILABLE` | Upstream failed and no usable cache exists |
| `STALE_DATA` | Served from cache older than the source's freshness policy; age reported |
| `UPSTREAM_ERROR` | Non-2xx, malformed body, or timeout (used with a cache fallback) |
| `PARSE_FAILED` | A record could not be parsed; raw text retained, record flagged, not dropped |
| `GEOMETRY_UNAVAILABLE` | Record has no spatial extent as published (e.g. text-only security TFRs); returned without geometry |
| `GEOMETRY_PARTIAL` | Some shapes unresolved (e.g. navaid-relative vertices) |
| `GEOMETRY_NOT_AREA` | Line feature (e.g. freezing level); a line has no inside |
| `ALTITUDE_UNKNOWN` | Missing or ambiguous vertical limits or datum; reported as unknown, never assumed |
| `SCHEDULE_UNEVALUABLE` | Record carries a schedule the server does not interpret; read it from the record |
| `UPSTREAM_FILTER_IGNORED` | Upstream ignores a filter; we do not substitute our own, the full published set is returned (info) |
| `UNDOCUMENTED_SOURCE` | Endpoint is not a documented, supported API (info) |
| `RESULT_TRUNCATED` | Output limited; count of omitted items given |
| `SOURCE_NOT_ENABLED` | Source configured off (e.g. NOTAMs pending FAA access) |
| `CLOCK_SKEW` | Upstream timestamps inconsistent with our clock |

We make no assumptions about applicability: unknown stays unknown, and the server never decides whether something applies to a flight. See the scope rule in §2.

## 2. Users and scope

Single-maintainer hobby project. Callers are AI agents, not people. US focus; international SIGMETs included because AWC provides them. No auth beyond a bearer token per agent plus a global rate limit.

### Scope: a data layer, not an advisor

The server's job is to make official aviation data **easily available and correct**. Deciding what matters for a particular flight is left to the end user and their agent.

**Rule (an application of §0): we only filter on what the official source itself filters on.** A wrong "this applies" costs some attention; a wrong "this doesn't apply" is a silent omission that could hide the one item that mattered. Any filter we wrote ourselves would turn our bugs into hidden gaps, so we do not write any.

In scope:
- Fetching, parsing and normalizing official data into structured form (format, not meaning), with the raw text preserved and the original source fields kept alongside any interpretation.
- Forwarding the filters an official API supports (e.g. the NMS-API's location, point-and-radius, effective-time and classification filters) and **saying in the response that the source applied them**. The filtering is the FAA's, not ours.
- Where the source offers no filter (AWC and the TFR feed give no usable area filter), returning the full published set, optionally narrowed only by exact match on a field the source itself publishes (e.g. a TFR's state, or an advisory's hazard).
- Being honest about gaps (§1).

Non-goals:
- Relevance scoring, ranking, prioritizing, summarizing or "you should care about this" guidance.
- Our own route, corridor or area intersection, "active at time T" decisions, altitude-overlap matching, or any label such as "applies", "possibly applies" or "does not apply". Agents that want this do it themselves from the geometry and fields we return.
- Suppressing records. Nothing is dropped; there is no default result cap.
- Flight-planning decisions or go/no-go advice.

Consequences: responses can be large, and the failure mode is visible (too much data) instead of hidden (a missing item). Every response states which filters the source applied and which, if any, we applied (`filters.applied_by_source`, `filters.applied_by_server`), so a caller can see exactly who narrowed the result.

## 3. Sources

| Need | Source | Access | Status |
|---|---|---|---|
| AIRMET (G-AIRMET), SIGMET, intl SIGMET, CWA | NOAA AWC Data API | public, no key | verified in spike |
| TFRs | **No documented API.** Authorized route: NMS-API (TFRs are FDC NOTAMs: text and dates, but on staging only a bare point, no outlines; 41 of 47 present). `tfr.faa.gov` publishes the outlines but blocks this client with HTTP 403 | the TFR adapter is built and tested on fixtures but **dormant** (not wired into any tool) | open: ask the FAA |
| NOTAMs (US) | FAA NOTAM Management Service API (NMS-API) | credentials by request: the FAA NMS page links "Request API Access" to 7-AWA-NAIMES@faa.gov (program questions: notams@faa.gov); staging access granted and verified (see SPIKE.md); production onboarding pending; ToS allow redistribution but not as "FAA data" |  staging verified |

## 4. Architecture

- **Go**, official MCP Go SDK (verify Streamable HTTP support), stdlib `net/http`, `log/slog`. Stdio for dev, Streamable HTTP for hosting.
- **No cache to start.** Every tool call fetches from the source live. Caching, polling and a snapshot store are premature until we measure a need (upstream load, latency or rate limits). Until then `served_from` is always `live`, and the `STALE_DATA` and `SOURCE_UNAVAILABLE` machinery is simply not exercised. If we add a cache later it must keep the same provenance contract (retrieval time, age, `served_from`).
- **Fetch whole feeds.** Upstream filters are unreliable (spike), and we do not replace them with our own. Only filters the source supports are forwarded (NMS-API); otherwise the full published set is returned.
- **TFR adapter:** read the TFR list, then each TFR's detail XML; normalize into the shared record model with the raw XML kept. Geometry is translated only where it is stated plainly (§4a); everything else is flagged.

```
cmd/hobbs/             main: wiring
internal/source/awc/   AWC adapters
internal/source/tfr/   FAA TFR adapter
internal/source/nms/   NMS-API NOTAM adapter
internal/model/        Advisory, Notam, Altitude, Geometry
internal/provenance/   Envelope, Source record, Issue codes, completeness rules
internal/geo/          translation of shapes into canonical geometry (no intersection logic)
internal/mcpserver/    tools, input validation, response shaping
```

Altitude is its own type: value, datum (MSL/AGL/FL/SFC/UNL), and an explicit "unknown" that is never zero. Time is UTC `time.Time` with an injectable clock. The server holds no logic that decides whether something is active or applicable.

## 4a. Geography: one representation

Every area, point and line the server returns is **one canonical form: an RFC 7946 GeoJSON geometry** (WGS84, positions as `[longitude, latitude]` in decimal degrees, longitude in [-180, 180], latitude in [-90, 90]). The five types are `Point`, `MultiPoint`, `LineString`, `Polygon` and `MultiPolygon`. Rings are closed and wound per the RFC (exterior counter-clockwise, holes clockwise). Whatever shape a source uses, the agent sees this.

Translation is deterministic, lives in `internal/geo`, and is tested with exact values. Each record carries `geometry_derivation` listing every translation applied, so the source's own shape is always recoverable.

| Source shape | Becomes | Derivation step |
|---|---|---|
| Circle (NOTAM Q-line point plus radius; later TFR circles) | `Polygon` (64-vertex geodesic on WGS84), or `MultiPolygon` across the antimeridian | `circle_to_circumscribed_polygon_64`; the circle as published is kept in `geometry_derivation.circle` |
| Point with no radius | `Point` | none |
| Several points (e.g. airway navaids) | `MultiPoint`, exactly as published (duplicates kept) | none |
| Longitudes beyond ±180 (AWC) | wrapped into [-180, 180]: a ring wholly beyond is shifted by 360°, one straddling it is cut at ±180 | `antimeridian_shift`, `antimeridian_cut` |
| Wrong winding | re-wound per the RFC | `ring_reoriented` |
| Multi-part area (AWC `AREAS`, GeoJSON `MultiPolygon`) | `MultiPolygon` | none |
| Zero-area part inside a multi-part area | dropped (no area is lost); never dropped if it has area | `degenerate_part_dropped` |
| Open path with no stated type | `LineString`, flagged | none |

The deliberate bias: where translation must approximate, it **overstates**. The circle polygon is circumscribed and padded, about 0.2% larger in radius (0.01 nm on a 5 nm circle), so an error shows as slightly extra data and never as a hidden miss.

Not translated, so the geometry is left empty and the record is flagged (`GEOMETRY_UNAVAILABLE` / `parse_issues`), never guessed: radius 999 (ICAO "no radius limit", i.e. a whole region, not a circle); circles near a pole (beyond 85° latitude) or over 500 nm; a polygon with holes that crosses the antimeridian; mixed point-and-polygon collections; a multi-part area with any unusable part; lines that cross the antimeridian.

This is translation only. There is no intersection, containment or "does this apply" logic anywhere (§0).

## 5. Tools (v1, built)

Three tools, all read-only, all returning the §1 envelope. There is deliberately no `check_route` and, for now, no `get_tfrs` (see §3 and SPIKE.md: no official TFR source we can use).

- **`get_notams`**: forwards the NMS-API's own filters (`location`, `latitude`/`longitude`/`radius_nm` up to 100, `effective_start`/`effective_end`, `last_updated`, `classification`, `feature`, `accountability`, `free_text`, `notam_number`). At least one filter is required. The response's `filters.applied_by_source` lists exactly what the FAA applied; `applied_by_server` is empty. Each NOTAM has its raw text in every rendering the source supplied, the published fields, canonical geometry (§4a) and `parse_issues`. TFRs appear as FDC NOTAMs with text and dates; outlines are not available from this source. Requests the API would reject (bad classification, a radius without a position, a malformed timestamp, no filter) are refused before anything is sent, with an error that names the problem.
- **`get_weather_advisories`**: the complete current SIGMET, graphical AIRMET, international SIGMET and CWA sets from NOAA AWC, optionally narrowed by `kinds` (which feeds to read) and an exact-match `hazard` applied by this server and declared in `applied_by_server`. Advisories come in a fixed feed order, each feed in the source's own order.
- **`source_status`**: checks every source live (no cache): reachable or not, latency, production versus staging, documented API or not.

Caller-chosen shaping, never silent: `limit` (no default cap; if items are omitted the response carries a `RESULT_TRUNCATED` warning and is not `complete`) and `include_geometry: false` (the response lists `omitted_fields: ["geometry"]`). A call that obtained no data at all returns `isError: true` together with the envelope; a partial failure returns the data plus a `critical` `SOURCE_UNAVAILABLE` issue and status `incomplete`.

Input errors come back as `isError` results with a message the agent can act on, and invalid NOTAM requests never reach the FAA.

### Transports and hosting

- **stdio** (default) for a local MCP client.
- **Streamable HTTP** (`-http :8080`, or just set `PORT`): stateless, at `/mcp`, with an open `/healthz`. It refuses to start without at least one bearer token (`HOBBS_TOKENS`, each at least 16 characters) and applies a global rate limit (`HOBBS_RATE`, `HOBBS_BURST`; default 5 per second, burst 10) so a runaway agent loop cannot exhaust upstream quotas. Unauthenticated requests never reach a source and do not consume the rate budget.
- Secrets only in environment variables (`HOBBS_TOKENS`, `NMS_CLIENT_ID`, `NMS_CLIENT_SECRET`); on Railway, as service variables. Logs go to stderr and never contain credentials.

## 6. Safety and trust

- **No guarantees.** The service is provided as is, with no warranty. We make a best effort to replicate official data as faithfully as we can, and we say plainly when we could not. We do not promise it is complete, timely or correct. Every response carries a disclaimer (`provenance.Disclaimer`) saying so, and tool descriptions repeat it.
- **Not presented as FAA data.** The NMS-API Terms of Service allow redistribution but say redistributed information may not be characterized as FAA data. The disclaimer states the data is a best-effort copy, not FAA/NWS data as published, and not endorsed. Sources are still named for provenance; confirm the attribution wording with the FAA if it becomes an issue.
- Advisory only. The FAA briefing channels remain the legal standard.
- NOTAM/TFR free text is untrusted: returned in a delimited `raw_text` field, never merged into summaries, and tool descriptions say it is data, not instructions.
- Hosted: bearer token per agent, global rate limit so a runaway loop cannot exhaust upstream quotas, kill switch for the NOTAM adapter, `/healthz` reporting per-source freshness, and an external uptime alert for stale feeds.
- Credentials (NMS client id/secret) only in environment variables.

## 7. Open items

- **TFRs, questions for the FAA** (ticket INC0036653, 7-AWA-NAIMES@faa.gov): (1) does NMS production publish TFR outlines (the NOTAM Team said in 2021 the geometry matches the TFR site), and if so in which format or filter? (2) why do 6 of the TFR site's 47 TFRs not appear in NMS staging, and will production have them? (3) may the TFR site's JSON/XML export be used programmatically, and if so how (it returns HTTP 403 to automated clients)? (4) is there a SWIM/SCDS digital-TFR service we may use?

- FAA NMS access and hosted-redistribution terms (email 7-AWA-NAIMES@faa.gov per the FAA NMS page, cc notams@faa.gov, as an individual hobby project).
- AWC documented rate limit and terms of use; FAA terms for `tfr.faa.gov` programmatic use.
- TFR geometry: parse only what is stated plainly; navaid-relative (`Frd`) vertices stay unresolved and flagged `GEOMETRY_PARTIAL`.
- `Add`/`Replace`/`Delete` semantics in TFR XML updates.
- Hosting target (Railway vs. cheap VPS).
