# Endpoint spike findings

Run 2026-11-22 against live services. Fixtures are in `testdata/`.
Everything here was observed, not assumed. Re-verify before relying on it; the TFR endpoints in particular are undocumented.

## AWC (aviationweather.gov) — NOAA, no key

Base: `https://aviationweather.gov/api/data/<name>?format=json`

| Endpoint | Items seen | `Cache-Control` | Notes |
|---|---|---|---|
| `airsigmet` | 1 | max-age=180 | US SIGMET + convective SIGMET (AIRMETs now come via `gairmet`) |
| `gairmet` | 34 | max-age=60 | Graphical AIRMETs: IFR, MT_OBSC, TURB-LO/HI, ICE, FZLVL, SFC_WND, LLWS |
| `isigmet` | 158 | max-age=180 | International SIGMETs, global (~160 KB) |
| `cwa` | 2 | max-age=60 | Center Weather Advisories |

- Plain JSON, ETag present, no auth. No rate-limit headers seen; AWC documents a limit (verify and honor it). Honor `Cache-Control` for polling cadence.
- **Server-side filters are unreliable.** `hazard=` filtered correctly (`hazard=turb` -> empty), but `bbox=` and `type=` were **ignored** on `airsigmet` (`bbox=0,0,1,1` still returned a NC SIGMET). Do all spatial/type filtering ourselves; fetch whole feeds and cache.
- Timestamp formats are inconsistent: epoch seconds (`validTimeFrom`, `issueTime`), ISO strings (`receiptTime`, `validTime`). Need per-feed decoding.
- Altitudes: `airsigmet` uses feet (`altitudeHi1`, nullable); `gairmet` uses `level`/`top`/`base`/`fzltop`; `isigmet`/`cwa` use `base`/`top`. All nullable. Null must mean "unknown", never zero.
- Coordinates: `[{lat, lon}]`. In `gairmet`/`cwa` the values are **strings**; in `airsigmet`/`isigmet` numbers. `gairmet` `geom` can be `LINE` (e.g. freezing level), not just polygons: lines don't have an inside.
- `gairmet?format=geojson` also works and gives proper GeoJSON geometry.
- Every item includes the raw official text (`rawAirSigmet`, `rawSigmet`, `rawText`), so provenance is easy. `gairmet` has no raw text.

### Added while building the AWC adapter (verified against live feeds)

- **Empty results are HTTP 204 with an empty body**, not `[]`. A bad parameter gives 400 with `{"status":"error","error":...}`; an unknown feed gives 404. A 200 with an empty body is therefore *not* how AWC says "nothing"; the adapter treats it as a failure.
- **Multi-part areas:** `isigmet` uses `geom: "AREAS"` with `coords` as a nested list of rings (7 of 160 records at the time). `coords` is otherwise a flat list of points.
- **Extended longitudes:** antimeridian-crossing features carry longitudes beyond 180 (e.g. 196.333) so each ring stays continuous. Kept as published (unwrapped); geometry code must handle it.
- **Altitudes have no stated datum** in any feed. Interpreted as feet (hundreds of feet for G-AIRMET), shown as a flight level at or above 18,000 ft. Every snapshot reports this as `ALTITUDE_ASSUMED`.
- **`isigmet` base 0 means SFC** (official text of WAAA 14 reads "SFC/FL070" while the record has `base: 0`). A null base is unknown.
- **G-AIRMET `base: "FZL"`** means the freezing level, which has no fixed height; kept unknown with the reference preserved in `details`.
- **G-AIRMET windows:** `validTime` to `expireTime` (= validTime + 3h) per forecast-hour snapshot.
- **Sloping limits** (`altitudeLow1/2`, `altitudeHi1/2`): the adapter takes the widest extent so a boundary is never understated.

## FAA TFRs (tfr.faa.gov) — undocumented

- `GET /tfrapi/getTfrList` — 47 TFRs, JSON, `Cache-Control: no-cache`. Fields: `notam_id`, `facility`, `state`, `type`, `description`, `mod_date`, `mod_abs_time`, `is_new`, `gid`. **No geometry, no altitudes, no dates beyond the free-text description.**
- `GET /download/detail_<id with / -> _>.xml` — full AIXM-ish `XNOTAM-Update` per TFR (all 47 fetched OK). Holds effective/expiry (`dateEffective`/`dateExpire`), altitudes, geometry, schedule and the full NOTAM text (`txtDescrUSNS`, `txtDescrTraditional`).
- `GET /tfrapi/getWebText?notamId=6/9306` — rendered HTML table; has links to `.xml`, `.aixm50`, and `.shp.zip`.
- `GET /tfrapi/exportTfrList` — same list plus `creation_date`.
- `getTfrDetail` does **not** exist (404). Don't guess endpoints; the ones above were observed.
- Responses start with a BOM (`﻿`); strip it before XML parsing.

### Geometry is the hard part

Of 47 TFRs:

- **12 (all `SECURITY`/`SPECIAL`) have no vertices at all.** They are text-only (e.g. stadium special-security instructions, "3 NMR of any qualifying stadium"). They apply by rule, not by polygon. **They must never be silently dropped**: surface them as `geometry: none` with full text, and have `check_route` report them as "not evaluable spatially".
- 25 contain `valRadius` (circles) and 5 contain `geoLatArc`; 4 contain `CWA` (clockwise arcs). These are rough substring counts, not parsed results: confirm with a real parser.
- 5 have multiple `TFRAreaGroup`s and 6 TFRs have 2-5 `aseTFRArea`s (separate altitude blocks and shapes).
- Vertices are written as `39.35990574N` / `077.03638889W` strings (not signed decimals).
- Some vertices are defined by radial/distance from a navaid (`Frd`: `valAngleBrg` + `valDist` from a VOR), not by lat/lon alone.
- Vertical limits use `codeDistVerLower/Upper` (`ALT`, `HEI`, ...), `valDistVer*`, `uomDistVer*`. Lower limit was `HEI` (AGL) in 37 of 47. MSL vs AGL must be modeled explicitly.
- Time is per area: `isScheduledTfrArea`, `ScheduleGroup` with daily start/end windows. "Active now" requires evaluating the schedule, not just effective/expire.
- A TFR can be a `Replace` / update chain; check `Add`/`Replace`/`Delete` handling.

## Consequences for the design

1. **Fetch whole feeds and filter locally.** Upstream filters can't be trusted. Poll on `Cache-Control` cadence (60-180s for AWC); cache is small, so memory is fine.
2. **TFR adapter = list + N detail fetches.** Poll the list, fetch XML only for new/changed IDs (`mod_abs_time`), cache by ID. List has no geometry, so a list-only result would be useless for `check_route`.
3. **Text-only TFRs are first-class.** Add `geometry_status: none | partial | full` and report it in every response. Never return a clean "no TFRs" when evaluable geometry is incomplete.
4. **Altitude type must carry datum and nullability** (MSL/AGL/FL/SFC/UNL/unknown). Highest-test-density package.
5. **Parser failures flag the record, not drop it.** Keep raw XML/text per TFR.
6. **Line geometries** (G-AIRMET freezing level) are not areas; treat as informational, not route-intersection hits.
7. **Skip `getTfrList` as the activity signal.** A listed TFR may be scheduled and inactive now; evaluate its schedule.

## Not yet verified

- AWC's actual rate limit and terms of use.
- FAA terms for programmatic use/redistribution of `tfr.faa.gov` (undocumented endpoints, could change without notice).
- NOTAMs (needs NMS credentials; not testable here).
- Navaid-relative (`Frd`) vertex resolution: need a navaid dataset, or skip and flag such TFRs as `partial`.
- `Delete`/`Replace` semantics across updates.
