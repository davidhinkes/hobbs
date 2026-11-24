# One tool for a flight: `get_flight_briefing_data` (design)

Date: 2026-11-24. Status: proposed, awaiting the maintainer's approval. It replaces an earlier proposal for a separate `get_weather_metar_taf` tool (same date, withdrawn by the maintainer: METARs and TAFs go into this single interface instead). Once built, the parts that last move into `docs/DESIGN.md` (§1, §3, §4, §5, §6, §8, §9), `README.md` and `CLAUDE.md`, as `CLAUDE.md` requires, and this file is deleted or marked superseded.

## 1. Intent

Today an agent briefing a flight calls `get_notams` and `get_weather_advisories` separately, with different inputs, and has no METARs or TAFs at all. The maintainer wants **one call with one uniform input that describes the flight**, translated by the server into each source's own query, and **one output** with a sublist per kind of data: NOTAMs, METARs, TAFs, and the weather advisories (SIGMETs, AIRMETs, international SIGMETs, CWAs).

Why one call: an agent cannot forget a piece (call the weather and never ask for the SIGMETs, say), and the whole briefing comes back under one provenance envelope.

The maintainer accepts that translating one input into several source queries is "a little bit of logic", on the condition that it is straightforward and testable. The design keeps it within `docs/DESIGN.md` §2 by three rules (§5.3 below): every translation is shown in the response, nothing a source cannot take is dropped without a flag, and nothing is inferred that the caller did not give.

## 2. Goals, non-goals, anti-goals

**Goals**

- `get_flight_briefing_data`: airports (ICAO), optional extra en-route centers, an optional time window, and a choice of what to include. One envelope, a sublist per kind.
- METARs and TAFs from NOAA AWC, new to hobbs.
- The tool **replaces** `get_notams` and `get_weather_advisories` (maintainer's decision, 2026-11-24). `source_status` stays (it is diagnostics, not data; the builder's call).

**Non-goals**

- A route, waypoints, or inferring the centers or stations between airports. `docs/DESIGN.md` §9 (2026-11-23) rejected route input, and `CLAUDE.md` forbids inferring centers; this design does not reopen either.
- An area (bounding-box) input for METARs/TAFs: deferred (the maintainer chose stations only to start). It can be added later without breaking callers.
- The narrower NOTAM and advisory queries the replaced tools offered (§4.4): point and radius, NOTAM number, free text, feature, classification, accountability, last-updated, advisory hazard. They go with the old tools.
- PIREPs, winds aloft, or any other product.

**Anti-goals (a failure even if it works)**

- Any relevance, "applies", "in effect now" or flight-category logic of our own.
- An input, or part of one, that a source cannot use and that disappears without a flag (a 3-letter identifier AWC does not match, an airport with no METAR, a time window a source cannot apply).
- A time window that hides NOTAMs the caller would need, without saying so.

## 3. Constraints

- `docs/DESIGN.md` §2 (principles), §3.3 R1 to R10, §5.1 (one envelope, built only through `respond` / `provenance.New`; status derived by rule).
- Official sources only; no cache (every call is live); the global rate limit on the hosted endpoint.
- `get_notams` today makes one FAA request per location plus one per implied center; this tool makes the same requests, plus up to six AWC requests (METAR, TAF, four advisory feeds).

## 4. Approaches considered

### 4.1 Data source for METARs and TAFs

| Option | Verdict |
|---|---|
| **NOAA AWC Data API** `/api/data/metar`, `/api/data/taf` (chosen) | Official, documented, no key, JSON; the same API and adapter as the advisories; source-applied `ids` and `hours`; raw report plus decoded fields (probed 2026-11-24, §7) |
| NWS `api.weather.gov` observations | Official, but no TAFs; a different model from the METAR as published |
| AWC bulk cache files | Whole-country files trimmed by us: a server-side filter §2 rules out |
| FAA sources | No public METAR/TAF API we know of |

### 4.2 Interface

| Option | Verdict |
|---|---|
| **One tool, one uniform input, replacing the data tools** (chosen by the maintainer) | Nothing can be forgotten; one envelope; one way to ask |
| One tool alongside the existing tools (proposed first) | Keeps the narrower queries, but two ways to ask, and the old tools still let an agent skip a piece |
| Separate tools per kind, adding `get_weather_metar_taf` (proposed first) | Withdrawn by the maintainer |

### 4.3 Input design (each decided by the maintainer, 2026-11-24)

| Question | Chosen | Alternatives (and why not) |
|---|---|---|
| How a flight is described | A list of airports | Bounding box: deferred; a station with no report cannot be flagged when there is no expected list |
| Identifier form | ICAO only | FAA 3-letter ids looked up in FAA NASR: correct, but a new data source to keep current. Adding a `K` ourselves: wrong in Alaska, Hawaii and the territories, and silent |
| Time | Optional `start_time` + `duration` | No time (every current NOTAM; visible over-inclusion): kept as the default when both are omitted |
| Name | `get_flight_briefing_data` (the builder's proposal, not objected to) | `get_weather`: reads as "all weather" next to the advisories tool, now moot |

### 4.4 What the replaced tools offered, and where it goes

| Old capability | In the new tool |
|---|---|
| `get_notams` `locations` (airports and centers), `add_centers` | `airports` and `centers`; implied centers always added (§5.3) |
| `effective_start` / `effective_end` | `start_time` / `duration` (§5.3) |
| `latitude` / `longitude` / `radius_nm`, `notam_number`, `free_text`, `feature`, `classification`, `accountability`, `last_updated` | dropped |
| `get_weather_advisories` `kinds` | `include` (§5.1) |
| `hazard` | dropped |
| `limit` (both tools) | dropped: with several sublists its meaning is unclear, and there is no default cap anyway (the builder's call) |
| `include_geometry` (both tools) | kept, default **false** here (§5.1) |

## 5. The design

### 5.1 Input

```jsonc
{
  "airports": ["KSMO", "KSNA"],             // required, 1 to 10 ICAO identifiers
  "centers": ["ZAB"],                        // optional: en-route centers the flight also crosses
  "start_time": "2026-11-26T14:00:00Z",      // optional, with duration
  "duration": "3h",                          // optional, with start_time
  "include": ["notams", "metars", "tafs", "sigmets", "airmets", "intl_sigmets", "cwas"], // default: all
  "include_geometry": false                  // optional, default false
}
```

- **`airports`**: exactly four letters or digits each, case-insensitive, returned upper case; duplicates removed. A 3-letter id (`SMO`) is an input error naming the ICAO form to use. AWC selectors such as `@CA` are rejected. At most 10, as `get_notams` today.
- **`centers`** (the builder's call, flagged for the maintainer): ARTCC identifiers (`ZAB`, or `KZAB`; the two count as the same, as today). Used for NOTAMs only. Without it, a flight that crosses a center its airports' NOTAMs do not list would lose that center's NOTAMs, which `get_notams` lets a caller add today (`docs/DESIGN.md` §3.2 F1 step 4). The response says centers between the airports are never inferred (the existing `locations.note`).
- **`start_time`** is a full RFC 3339 UTC timestamp. A bare time (`14:00Z`) is an input error: today or tomorrow would be a guess. **`duration`** is a positive Go-style duration in hours and minutes (`3h`, `90m`, `1h30m`), at most 72h (the builder's call, to be checked against the FAA's limits). Both or neither.
- **`include`** chooses sublists; the default is all seven. METARs and TAFs are separate entries so a caller can ask for one. The advisory kinds map to the existing feeds (SIGMET, AIRMET, INTL_SIGMET, CWA).
- **`include_geometry`** defaults to **false** in this tool, unlike the old tools (the builder's call): a KSMO/KSAN briefing with geometry would be roughly 300 KB of JSON, about twice that on the wire. The response lists `geometry` under `omitted_fields`, so the omission is visible.

### 5.2 Output

```jsonc
{
  "data": {
    "request":     { /* the input as understood: upper-cased, de-duplicated */ },
    "translation": [ /* §5.3: what each source was asked, and what it was not */ ],
    "locations":   { "requested": [...], "added": [...], "note": "..." },  // as get_notams today
    "omitted_fields": ["geometry"],
    "notams":       [ /* model.Notam, as today */ ],
    "metars":       [ /* station report, §5.4 */ ],
    "tafs":         [ /* station report, §5.4 */ ],
    "sigmets":      [ /* model.Advisory, as today */ ],
    "airmets":      [ ... ],
    "intl_sigmets": [ ... ],
    "cwas":         [ ... ]
  },
  "evaluated_at": "...", "provenance": { ... }, "completeness": { ... },
  "airworthy": false, "disclaimer": "..."
}
```

A sublist not in `include` is omitted, and `translation` says it was not requested. Each sublist is in the source's order. Records are exactly the existing NOTAM and advisory models, plus the new station report.

One envelope covers everything: each upstream request is its own provenance source (`faa.nms.notam:KSMO`, `faa.nms.notam:ZLA`, `awc.metar`, `awc.taf`, `awc.airsigmet`, ...), so a failed source makes the response `incomplete` without hiding the others. If every source fails, `isError: true` with the envelope, as today.

### 5.3 Translation, per source

The translation is a pure function from the validated input to a list of source queries, and it is returned in the response (`translation`), one entry per source:

| Source | What it is asked | Time window | Not used / flagged |
|---|---|---|---|
| FAA NMS NOTAMs | `location` = each airport, each `centers` entry, then each implied center the FAA lists on those NOTAMs (one round, as `get_notams` today) | `effectiveStartDate` / `effectiveEndDate` = start, start + duration, applied by the FAA, **if the probe in §6 passes** | — |
| AWC METAR | `ids` = the airports | not applied: latest METAR per station | each airport with no METAR: `STATION_NO_DATA` |
| AWC TAF | `ids` = the airports | not applied: latest TAF, every period as published | each airport with no TAF: `STATION_NO_DATA` |
| AWC advisories (four feeds) | the full current set, as today | not applied | not filtered by airport: said in `translation` |
| (centers) | NOTAMs only | | `translation` says METAR/TAF were not asked for centers |

Rules the translation keeps (and the tests check):

1. **Shown.** Every source query appears in `translation` with the parameters actually sent, who applies each filter (`applied_by_source`; `applied_by_server` stays empty), and which parts of the input it did not use.
2. **Nothing dropped quietly.** Anything a source cannot take gets an issue (`STATION_NO_DATA`; `TIME_WINDOW_NOT_APPLIED`, info, for each source the window was not sent to).
3. **Nothing inferred.** No places, times or identifiers the caller did not give; the one addition, the implied center, uses only the FAA's own association, as today.

### 5.4 METARs and TAFs: the station report

One model type for both:

- `id`: built from published fields, e.g. `METAR KSNA 2026-11-24T05:53:00Z` (kind, station, observation or issue time).
- `kind` (`METAR` or `TAF`), `report_type` as published (`METAR`/`SPECI` from `metarType`).
- `station`, `station_name`, the station elevation as published (`elev`; the builder confirms its unit and names the field for it), and `geometry`: a GeoJSON `Point` at the published `lat`/`lon`.
- Times as RFC 3339 UTC (AWC mixes epoch seconds and ISO strings): METAR `observed_at`, `reported_at`, `received_at`; TAF `issued_at`, `valid_from`, `valid_to`.
- `raw_text` (`rawOb` / `rawTAF`, unaltered); `raw_format` gains two values, `metar` (METAR and SPECI) and `taf`.
- `details`: every other AWC field as published, including `fltCat` (AWC's flight category; the description says it is AWC's, not computed by hobbs), `visib` (e.g. `"10+"`), `altim` (hPa, although the raw METAR gives inches), `clouds`, and the TAF's `fcsts` periods.
- `parse_issues` and `source_record` as for other records; an unparseable record is kept and flagged `PARSE_FAILED`.

Converting units or normalizing cloud layers into our own fields was considered and left out: each conversion is an interpretation to document and flag, and AWC's decoding is already kept whole. It can be added later under §2.1, with the originals kept.

### 5.5 New issue codes

| Code | Meaning |
|---|---|
| `STATION_NO_DATA` | A requested airport returned no METAR (or no TAF). AWC answers the same way for "no current report", "no TAF published here" and "unknown station", so it is not a "none" (warning) |
| `TIME_WINDOW_NOT_APPLIED` | A time window was given but this source was not filtered by it; its records are current, not for the window (info) |

### 5.6 `source_status`

Stays, and adds `awc.metar` and `awc.taf` checks (one well-known station each).

## 6. Must be verified before the time window is used

The FAA's effective-window filter on staging, probed and recorded in `docs/DESIGN.md` §5.5 before the window is sent:

- Does it return NOTAMs that **overlap** the window (start before it, end after it)?
- Does it return **permanent** NOTAMs (`PERM`) and NOTAMs with an **estimated** end (`EST`)?
- Its maximum window, if any.

If it returns only NOTAMs entirely inside the window, or drops PERM or EST NOTAMs, it would hide NOTAMs that matter. Then the window is **not** sent to the FAA: `translation` and a `TIME_WINDOW_NOT_APPLIED` issue say so, and every current NOTAM is returned. That fallback is a visible over-inclusion, which §2 accepts.

## 7. What AWC was observed to do (probed live, 2026-11-24)

These go into `docs/DESIGN.md` §5.5 with the build.

- `ids=KSMO,KSNA` returns exactly those stations; lower case works; `hours=3` gave 3 METARs, `hours=48` gave 48. The TAF endpoint rejects `hours` (HTTP 400).
- `bbox=` works on these endpoints (unlike the advisory feeds, 2026-11-22): a box over the LA basin returned 10 METAR and 5 TAF stations. Not used.
- Empty results differ: METAR answers HTTP 204 with an empty body; TAF answers 200 with `[]`. Both mean "no records". The existing rule (a 200 with an empty body is a failure) still holds.
- An unknown station (`KXYZ`) and a 3-letter id (`SMO`) get the empty answer, the same as "no current report". `@CA` returns a whole state.
- `Cache-Control: max-age=60` on METAR; no rate-limit headers.
- Fields: epoch seconds (`obsTime`, `validTimeFrom`) beside ISO strings (`reportTime`, `issueTime`); `visib` as a string (`"10+"`, `"6+"`); `altim` in hPa (`1021.8`) while the raw text says `A3017`; cloud `base` in feet; TAF periods in `fcsts` with their own epoch times and `fcstChange` (`FM`, `BECMG`, ...).

## 8. What else changes

- **Removed:** `get_notams` and `get_weather_advisories`, their tests and descriptions. A breaking change for any client that calls them (the `.mcp.json` session tools, anything external). Record it in `docs/DESIGN.md` §9 with the alternatives in §4.2.
- **`docs/DESIGN.md`:** §1 status; §3.2 flows (F1 and F3 become one flight flow; F2, NOTAMs around a point, is removed); §3.4 non-goals (bbox deferred, narrower queries dropped); §4 diagram and request walk-through; §5.1 (`raw_format` values, new issue codes); §5.2 (the tool); §5.3 (the station report); §5.5 (§6 and §7 findings); §5.7 code layout; §6 tests; §8 open items (bbox, the narrower queries if ever wanted again); §9 entries: one tool replacing the data tools, ICAO-only input, the time window, METAR/TAF from AWC, `include_geometry` default false here, `limit` dropped.
- **`README.md`:** the tools. **`CLAUDE.md`:** the end-to-end test tools (`mcp__hobbs__get_flight_briefing_data`, `mcp__hobbs__source_status`).
- **Tests:** offline fixtures for METAR, SPECI, TAF, the 204 and `[]` empties, a missing station, a malformed record; table tests for the translation (input to source queries, including centers, the window and its fallback); the envelope test over the new tool; a `live` integration test; end-to-end through the hosted server after deploy.

## 9. Left to the builder

Package layout (likely `internal/mcpserver/briefing.go` reusing the NOTAM and advisory code paths; `internal/source/awc` gains the two feeds; `internal/model` the station report), Go names, the `translation` JSON shape within §5.3, the `source_status` probe stations, the elevation unit name, the tool description's wording (it must state the not-airworthy, completeness and untrusted-text language, and that the server decides nothing about relevance), and parallelism across sources. Each choice made goes into `docs/DESIGN.md` where it lasts.
