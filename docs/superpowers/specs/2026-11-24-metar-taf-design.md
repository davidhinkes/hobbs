# METARs and TAFs: design

Date: 2026-11-24. Status: proposed, awaiting the maintainer's approval. Once built, the parts that last move into `docs/DESIGN.md` (§3, §4, §5, §9) and `README.md`, as `CLAUDE.md` requires.

## 1. Intent

Hobbs makes official US aviation data available to agents (NOTAMs and weather advisories today). A briefing also needs **current conditions and forecasts at the airports of a flight**: METARs (observations) and TAFs (terminal forecasts). This adds them, under the same principles (`docs/DESIGN.md` §2): the data as published, uniform, with provenance, and no decisions for the user.

The first user flow is a flight briefing: an agent that already has the departure, destination and alternates asks for their METARs and TAFs in one call, alongside `get_notams` and `get_weather_advisories`.

## 2. Goals, non-goals, anti-goals

**Goals**

- One tool returns METARs and/or TAFs for a list of stations the caller names.
- Each report carries its raw text as published, AWC's own decoded fields, and the usual envelope (sources, retrieval times, every data-pull problem, `airworthy`).
- A requested station that returns nothing is flagged, so "no report" never reads as a clean "none".

**Non-goals (for this version)**

- Area queries (AWC's `bbox`). The maintainer chose stations only to start; `bbox` can be added later without breaking callers (see §4).
- PIREPs, winds aloft, area forecasts, or any other AWC product.
- Our own METAR or TAF decoder.

**Anti-goals (a failure even if it works)**

- Deciding anything: which TAF period is "in effect now", whether conditions are VFR or IFR by our own rule, which station is "nearest", whether a report matters.
- A station silently missing from the answer (a missing report, an unknown identifier, a 3-letter identifier AWC does not match) with nothing to say so.
- A name or description that lets an agent think this tool covers SIGMETs and AIRMETs, so it skips `get_weather_advisories`.

## 3. Constraints

- `docs/DESIGN.md` §2 (no relevance, applicability or "active now" logic; filter only with the source's own filters), §3.3 R1 to R10, and §5.1 (the envelope, built only through `respond` / `provenance.New`).
- Official sources only (R3).
- No cache: every call fetches live (§9, 2026-11-22).
- The global rate limit on the hosted endpoint applies to the new tool too.

## 4. Approaches considered

**Data source**

| Option | Verdict |
|---|---|
| **NOAA AWC Data API** `/api/data/metar` and `/api/data/taf` (chosen) | Official, documented, no key, JSON; the same API and adapter style as the advisories; source-applied `ids` and `hours` filters; publishes the raw report and decoded fields. Probed live 2026-11-24 (§6) |
| NWS `api.weather.gov` station observations | Official, but has no TAFs, so a second source would still be needed; its observations are a different model from the METAR as published |
| AWC bulk cache files (`/data/cache/metars.cache.xml.gz`) | Whole-country downloads to trim ourselves: a server-side filter §2 rules out, and much more data per call |
| FAA sources | No public METAR/TAF API we know of |

**Query input** (decided by the maintainer, 2026-11-24)

| Option | Verdict |
|---|---|
| **A. Stations only** (chosen) | Matches how `get_notams` briefs a flight; the server knows what was asked, so it can flag every station that returned nothing |
| B. Bounding box only | Finds stations the agent did not name, but a station with no report simply does not appear: no expected list, so nothing can flag it |
| C. Both, either one required | Proposed as the eventual shape; deferred. Adding `bbox` later is additive. Whether AWC ANDs or ORs `ids` with `bbox` is untested |

**Tool shape** (decided by the maintainer, 2026-11-24)

| Option | Verdict |
|---|---|
| **One tool with `kinds`** (chosen) | Mirrors `get_weather_advisories`; a briefing is one call; a missing TAF is flagged in the same answer as the METAR |
| Two tools, `get_metars` and `get_tafs` | Two calls, and easier to forget the TAF |
| Fold into `get_weather_advisories` | Advisories are a national set, METARs/TAFs are per station; mixing them muddles both tools' inputs |

**Name** (decided by the maintainer, 2026-11-24): `get_weather_metar_taf`. It groups with `get_weather_advisories` and says exactly what it returns. `get_weather` was considered and set aside: next to `get_weather_advisories`, an agent could read it as "all weather" and skip the advisories (an anti-goal). `get_station_weather` was the other candidate.

**Record content**

| Option | Verdict |
|---|---|
| **Raw text, a few normalized common fields, AWC's decoded fields kept as published in `details`** (chosen, the builder's call below) | The advisory model's pattern (`details`: "source fields with no common meaning, kept as published"); least interpretation; nothing is lost |
| Fully normalized fields (visibility as a number, pressure in one unit, cloud layers in our own schema) | More convenient, but every conversion is an interpretation to document and flag (AWC publishes visibility as `"10+"` and altimeter in hPa although the METAR says inches); can be added later as §2.1 normalizations, with the originals kept |
| Our own decoder from the raw text | Duplicates AWC's decoding and adds a new way to be wrong |

## 5. The design

### 5.1 Tool: `get_weather_metar_taf`

Inputs:

| Parameter | Meaning |
|---|---|
| `stations` (required) | 1 to 20 four-character ICAO station identifiers, e.g. `["KSMO", "KSNA"]`. Case-insensitive; returned upper case. Anything else is an input error (§5.4) |
| `kinds` | `METAR`, `TAF`, or both (default both) |
| `hours` | METAR only, optional, 1 to 48: every METAR AWC has for the past N hours, applied by AWC (`hours=`). Without it, AWC returns the latest METAR per station. Given without `METAR` in `kinds`, it is an input error, never silently dropped |
| `limit` | As in the other data tools: no default cap; omitted items give `RESULT_TRUNCATED` |

`include_geometry` is not offered: a report's only geometry is its station point (§5.3), which is small.

The description must say, in its first lines, that it returns **METARs and TAFs only, not SIGMETs, AIRMETs or CWAs (use `get_weather_advisories`)**, and carry the same not-airworthy, completeness and untrusted-text language as the other tools.

Behavior:

- One AWC request per kind, with all stations in `ids=` (e.g. `/api/data/metar?ids=KSMO,KSNA&format=json`). Each is its own provenance source: `awc.metar`, `awc.taf`. A failed kind makes the response `incomplete` without hiding the other (`SOURCE_UNAVAILABLE`, critical); if both fail, `isError: true` with the envelope, as for the other tools.
- `filters.applied_by_source` lists `ids` (and `hours`) per source; `applied_by_server` is empty.
- Records are returned in the source's order, METARs first, then TAFs. Nothing is merged, ranked or dropped. TAFs come with every period as published; no "current period" logic.
- **Every requested station with no record of a kind** gets a `STATION_NO_DATA` warning (new code, §5.2), one issue per kind, listing the stations in `refs`. The message says AWC does not distinguish "no current report" from "no such station" and that many airports publish no TAF.

### 5.2 New issue code

| Code | Meaning |
|---|---|
| `STATION_NO_DATA` | A requested station returned no record of this kind. AWC answers the same way for "no current report" and "unknown station", so this is not a "none" (warning) |

### 5.3 Data model: the station report

One new model type for both kinds (field names follow the existing models):

- `id`: a stable identifier built from source fields, e.g. `METAR KSNA 2026-11-24T05:53:00Z` or `TAF KSNA 2026-11-24T05:24:00Z` (kind, station, observation or issue time).
- `kind`: `METAR` or `TAF`. `report_type`: as published (`METAR` or `SPECI` from `metarType`; TAF amendments as published, if AWC marks them).
- `station`, `station_name`, `elevation_m`... as published by AWC (`icaoId`, `name`, `elev`; the builder confirms `elev`'s unit and says so in the field name), and `geometry`: a GeoJSON `Point` at the station's published `lat`/`lon` (R6).
- Times, normalized to RFC 3339 UTC (§2.1; AWC mixes epoch seconds and ISO strings): METAR `observed_at` (`obsTime`), `reported_at` (`reportTime`), `received_at` (`receiptTime`); TAF `issued_at` (`issueTime`), `valid_from` / `valid_to` (`validTimeFrom` / `validTimeTo`).
- `raw_text` (`rawOb` / `rawTAF`, unaltered) and `raw_format`: two new values, `metar` (METAR and SPECI) and `taf`. This extends the `raw_format` set in §5.1 rule 6 of `docs/DESIGN.md`.
- `details`: every other AWC field, kept as published, including `fltCat` (AWC's flight category), `visib`, `altim`, `clouds`, and the TAF's `fcsts` periods. The tool description and `docs/DESIGN.md` say `fltCat` is AWC's, not computed by hobbs.
- `parse_issues` and `source_record` exactly as for other records: a record that cannot be parsed is kept, flagged `PARSE_FAILED`, with its source record.

### 5.4 Input validation (requests that never reach AWC)

- Station identifiers must be exactly four letters or digits. A 3-letter FAA identifier (`SMO`) returns nothing from AWC (HTTP 204), so it is rejected with a message naming the ICAO form to use. The server does not add a `K` itself: that guess is wrong in Alaska, Hawaii and the territories.
- AWC selectors that are not station identifiers (`@CA` returns a whole state; probed 2026-11-24) are rejected.
- More than 20 stations, an empty list, duplicates after upper-casing (de-duplicated, not an error; the builder's call), `hours` outside 1 to 48, or `hours` without `METAR` are input errors.

### 5.5 `source_status`

Adds `awc.metar` and `awc.taf` checks (a cheap request, e.g. one well-known station), like the existing AWC feeds.

## 6. What AWC was observed to do (probed live, 2026-11-24)

These go into `docs/DESIGN.md` §5.5 with the build.

- `ids=KSMO,KSNA` returns exactly those stations; lower case works. `hours=3` returned 3 METARs, `hours=48` returned 48.
- `bbox=` **works** on these endpoints (unlike the advisory feeds, where it was ignored on 2026-11-22): a box over the LA basin returned 10 METAR and 5 TAF stations. Not used in this version.
- **Empty results differ by endpoint:** METAR answers HTTP 204 with an empty body; TAF answers HTTP 200 with `[]`. Both mean "no records". The existing AWC rule (a 200 with an empty body is a failure) still holds; `[]` is not an empty body.
- An unknown station (`KXYZ`) and a 3-letter identifier (`SMO`) both get the empty answer, indistinguishable from "no current report". `@CA` returns a state's stations.
- The TAF endpoint rejects `hours` (HTTP 400).
- `Cache-Control: max-age=60` on METAR. No rate-limit headers seen.
- Field shapes: times as epoch seconds (`obsTime`, `validTimeFrom`) and ISO strings (`reportTime`, `issueTime`); `visib` as a string such as `"10+"` or `"6+"`; `altim` in hPa (`1021.8`) although the raw METAR gives inches (`A3017`); cloud `base` in feet; TAF periods in `fcsts` with their own epoch times and `fcstChange` (`FM`, `BECMG`, ...).

## 7. Docs and tests that change with it

- `docs/DESIGN.md`: §1 and §3 (a new flow F7, METARs/TAFs for a flight), §3.4 (bbox deferred), §4 diagram, §5.1 (new `raw_format` values, `STATION_NO_DATA`), §5.2 (the tool), §5.3 (the model), §5.5 (the observations above), §5.7 (code layout), §9 (decision entries: the source, stations only, one tool and its name, record content).
- `README.md`: the tools list. Tool descriptions and package comments in the code.
- Tests: offline fixtures in `testdata/` for METAR, SPECI, TAF, the 204 and `[]` empties, a missing station, a malformed record; the envelope test that covers every tool; the shared-parameter-description test (`limit`); a `live` integration test. End-to-end through the hosted server after deploy (`CLAUDE.md`).

## 8. Left to the builder

Package layout (likely `internal/source/awc` gains the two feeds; `internal/model` gains the report type), exact Go names, the `source_status` probe station, the unit name for elevation, fixture content, and the wording of the tool description within the constraints above. Each choice made goes into `docs/DESIGN.md` where it lasts.
