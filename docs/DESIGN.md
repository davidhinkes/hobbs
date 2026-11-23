# Hobbs: aviation advisories MCP server — design

Status: draft. Evidence for source behavior is in `docs/SPIKE.md`.

## 1. Core principle: provenance and honesty about data quality

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
| `GEOMETRY_UNAVAILABLE` | Record has no spatial extent (e.g. text-only security TFRs); cannot be route-checked |
| `GEOMETRY_PARTIAL` | Some shapes unresolved (e.g. navaid-relative vertices) |
| `GEOMETRY_NOT_AREA` | Line feature (e.g. freezing level); informational, not intersected |
| `ALTITUDE_UNKNOWN` | Missing/ambiguous vertical limits or datum; treated conservatively |
| `SCHEDULE_UNEVALUABLE` | TFR schedule could not be evaluated for the requested time |
| `UPSTREAM_FILTER_IGNORED` | Upstream ignores a parameter; we filtered locally (info) |
| `UNDOCUMENTED_SOURCE` | Endpoint is not a documented, supported API (info) |
| `RESULT_TRUNCATED` | Output limited; count of omitted items given |
| `SOURCE_NOT_ENABLED` | Source configured off (e.g. NOTAMs pending FAA access) |
| `CLOCK_SKEW` | Upstream timestamps inconsistent with our clock |

Conservative defaults: unknown altitude means "may apply at any altitude"; an unevaluable schedule means "may be active". Both are flagged.

## 2. Users and scope

Single-maintainer hobby project. Callers are AI agents, not people. US focus; international SIGMETs included because AWC provides them. No auth beyond a bearer token per agent plus a global rate limit.

### Scope: a data layer, not an advisor

The server's job is to make official aviation data **easily available and correct**. Deciding what matters for a particular flight is left to the end user and their agent.

In scope:
- Fetching, parsing and normalizing official data into structured, queryable form, with raw text preserved.
- Objective, deterministic queries: by location, area, route corridor, altitude, and time (active/effective at T). These are filters on stated facts (does this polygon intersect this corridor, is this window active at this time), not judgments of importance.
- Being honest about gaps (§1).

Non-goals:
- Relevance scoring, ranking, prioritizing, summarizing or "you should care about this" guidance.
- Suppressing or hiding records because they seem unimportant. Within a query's objective filters, everything matching is returned.
- Flight-planning decisions or go/no-go advice.

Consequences: results are complete within the query, in a stable and documented order (e.g. by effective time), and carry source IDs, raw text and structured fields so a downstream agent can do its own reasoning. Where we cannot evaluate a record against a filter (no geometry, unknown altitude or schedule), it is **included and flagged**, not excluded.

## 3. Sources

| Need | Source | Access | Status |
|---|---|---|---|
| AIRMET (G-AIRMET), SIGMET, intl SIGMET, CWA | NOAA AWC Data API | public, no key | verified in spike |
| TFRs | FAA `tfr.faa.gov` list + per-TFR XML | public, undocumented | verified in spike |
| NOTAMs (US) | FAA NOTAM Management Service API (NMS-API) | credentials by request: the FAA NMS page links "Request API Access" to 7-AWA-NAIMES@faa.gov (program questions: notams@faa.gov); staging access granted and verified (see SPIKE.md); production onboarding pending; ToS allow redistribution but not as "FAA data" |  staging verified |

## 4. Architecture

- **Go**, official MCP Go SDK (verify Streamable HTTP support), stdlib `net/http`, `paulmach/orb` for geometry, `log/slog`. Stdio for dev, Streamable HTTP for hosting.
- **Pollers, not pass-through.** One background poller per source on its `Cache-Control` cadence (AWC 60–180 s), with jitter and `singleflight`. Tool calls read the cache, so upstream load is constant regardless of callers.
- **Snapshots carry provenance** (retrieved_at, upstream timestamp, ETag, hash, HTTP status, issues). The cache keeps the last good snapshot plus the most recent failure, so a failed fetch degrades to a flagged stale result.
- **TFR adapter:** poll `getTfrList`; fetch `detail_<id>.xml` only for new or changed IDs (`mod_abs_time`); parse into normalized records; keep raw XML.
- **Fetch whole feeds, filter locally.** Upstream filters are unreliable (spike).
- **Snapshot file on disk** so a restart does not begin with an empty cache; restored data keeps its original `retrieved_at`.

```
cmd/hobbs/             main: wiring
internal/source/awc/   AWC adapters
internal/source/tfr/   FAA TFR adapter
internal/source/nms/   NOTAM adapter (feature-flagged)
internal/model/        Advisory, Notam, Altitude, Geometry
internal/provenance/   Envelope, Source record, Issue codes, completeness rules
internal/geo/          intersection, corridor, altitude overlap (geodesic)
internal/cache/        snapshots, stale-while-error
internal/mcpserver/    tools, input validation, response shaping
```

Altitude is its own type: value, unit, datum (MSL/AGL/FL/SFC/UNL), and a nullable "unknown". Time is UTC `time.Time` with half-open intervals and an injectable clock.

## 5. Tools (v1)

- `check_route(waypoints, altitude, departure_time)`: the main entry point. Intersects route/corridor with TFRs, SIGMETs, G-AIRMETs and CWAs; lists text-only TFRs it could not evaluate.
- `get_tfrs`, `get_airsigmets`: lower-level reads by area/point/time.
- `get_notams`: US NOTAMs, behind a feature flag until FAA terms allow it.
- `source_status`: per-source freshness, last success/failure, endpoint documentation status.

Few tools, structured compact JSON, strict input validation with field-specific errors, pagination with explicit truncation.

## 6. Safety and trust

- **No guarantees.** The service is provided as is, with no warranty. We make a best effort to replicate official data as faithfully as we can, and we say plainly when we could not. We do not promise it is complete, timely or correct. Every response carries a disclaimer (`provenance.Disclaimer`) saying so, and tool descriptions repeat it.
- **Not presented as FAA data.** The NMS-API Terms of Service allow redistribution but say redistributed information may not be characterized as FAA data. The disclaimer states the data is a best-effort copy, not FAA/NWS data as published, and not endorsed. Sources are still named for provenance; confirm the attribution wording with the FAA if it becomes an issue.
- Advisory only. The FAA briefing channels remain the legal standard.
- NOTAM/TFR free text is untrusted: returned in a delimited `raw_text` field, never merged into summaries, and tool descriptions say it is data, not instructions.
- Hosted: bearer token per agent, global rate limit so a runaway loop cannot exhaust upstream quotas, kill switch for the NOTAM adapter, `/healthz` reporting per-source freshness, and an external uptime alert for stale feeds.
- Credentials (NMS client id/secret) only in environment variables.

## 7. Open items

- FAA NMS access and hosted-redistribution terms (email 7-AWA-NAIMES@faa.gov per the FAA NMS page, cc notams@faa.gov, as an individual hobby project).
- AWC documented rate limit and terms of use; FAA terms for `tfr.faa.gov` programmatic use.
- Navaid dataset for `Frd` vertices, or flag those TFRs `GEOMETRY_PARTIAL`.
- `Add`/`Replace`/`Delete` semantics in TFR XML updates.
- Hosting target (Railway vs. cheap VPS).
