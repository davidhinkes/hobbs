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

## FAA NMS-API (NOTAMs) — verified against staging, 2026-11-22

Access: requested by email per the FAA NMS page; the FAA issued staging credentials (OAuth2 client credentials) and an OpenAPI spec (v1.0.18). Production onboarding is a separate step through the NOTAM Service Center. Terms of Service apply (redistribution allowed but not as "FAA data"; no warranty; indemnification clause).

- **Hosts:** staging `https://api-staging.cgifederal-aim.com` (token at `/v1/auth/token`, API at `/nmsapi/v1/...`); production per the spec `https://api-nms.aim.faa.gov/nmsapi`. The token endpoint is outside the `/nmsapi` path.
- **Auth:** `POST /v1/auth/token` with HTTP Basic (client id/secret) and `grant_type=client_credentials`. Bearer token lasts 1799 s; keys do not expire. A 401 `{"status":"401","message":"Access Token is Invalid or Expired"}` means the *token* expired; bad credentials return `invalid_client`.
- **Request:** `GET /nmsapi/v1/notams` with required header `nmsResponseFormat: GEOJSON|AIXM`, plus filters (AND-ed): `location`, `latitude`+`longitude`+`radius` (radius 0-100 nm; all three together), `effectiveStartDate`+`effectiveEndDate` (together), `lastUpdatedDate` (limited to the prior 24 h), `classification`, `accountability`, `feature`, `freeText`, `notamNumber` (needs location or accountability), `nmsId`. No filters gives 400. `classification` alone returns a short-lived download link, not data.
- **Zero results:** HTTP 200 `{"status":"Success","data":{}}` (no `geojson` key), unlike AWC's 204. Errors: 400 `{"status":"Failure","message":...}`, 401 as above (empty body when no auth at all).
- **Response:** `data.geojson` is a list of Features. `properties.coreNOTAMData` holds `notam` (id, number, year, type, issued, affectedFir, selectionCode, minimumFl/maximumFl, location, icaoLocation, effectiveStart/End, text, classification, accountId, lastUpdated, coordinates, radius, series, purpose, traffic, scope, estimated, lowerLimit, upperLimit, schedule) and `notamTranslation` (`ICAO` `formattedText` and/or `LOCAL_FORMAT` `simpleText`). Geometry is a GeometryCollection of Points or Polygons.
- **Variation seen** (181 records for KIAD, 540 within 25 nm): `type` missing on ~3-11%; `effectiveEnd` is `"PERM"` (13-44 records) or missing (1); `estimated` is `"true"`/`"True"`; `lowerLimit`/`upperLimit` are strings like `SFC`, `UNL`, `FL200`, `6000FT AMSL`, `100FT AGL`, and bare `135FT` (no datum, mostly obstacles); minimumFl/maximumFl `000`/`999` mean no vertical limit stated; `radius` is `"005"` or `"5"`; Q-line `coordinates` are `3856N07727W`; some records have only one translation; no polygon holes seen; mixed point/polygon geometry not seen.
- **Staging is not operational:** the data includes entries like "AIRSPACE TESTED FOR CUTOVER SIMULATION". The adapter flags every non-production snapshot with `NON_PRODUCTION_SOURCE`.
- **Volume:** a single airport returns hundreds of KB; no rate-limit headers were seen, limits are unknown. The FAQ suggests polling "the last 3 minutes".

### Found while unifying geography (live data)

- **AWC `AREAS` can contain a degenerate part.** Live `isigmet` SBGL 95 had a second part with only two distinct points (zero area) that the official text does not describe. Dropped, and recorded as `degenerate_part_dropped`.
- **NMS FDC route NOTAMs have several Points and no area** (airway restrictions between VORs; one repeated the same two points). Represented as `MultiPoint`, as published.
- **A NOTAM `radius` of 999** means "no radius limit" in ICAO usage, not a 999 nm circle; no geometry is derived.
- A 100 nm query around IAD returned 1,335 NOTAMs: 906 polygons, 402 points (756 of the total were circles from point plus radius), 21 multi-part areas, 6 multi-point; no polygon holes seen.

### TFR site blocks this client (2026-11-23)

The TFR adapter (`internal/source/tfr`) was built and tested against captured fixtures, and its detail format was verified by hand with `curl`. When the Go adapter itself called `tfr.faa.gov` live, the site answered **HTTP 403 "Access Denied"**, including for the list endpoint that `curl` had fetched successfully. That is an access control on the FAA's edge, not a bug we can fix in the parser. We did not try to get around it (varying headers or connection settings to look like another client): this is an undocumented endpoint and the block reads as intentional. Consequences:

- The TFR adapter cannot be used live from this client until the FAA agrees to it. Ask the FAA (the TFR site is run by the same NOTAM organization) whether and how automated access is permitted.
- The authorized alternative already in hand is the NMS-API: TFRs are FDC NOTAMs, which NMS serves with structured fields and geometry (e.g. `classification=FDC`). That would need verifying against what the TFR site publishes before relying on it.

### Is there a sanctioned way to pull TFRs? (2026-11-23)

Short answer: there is no documented public TFR API. What the FAA's own materials say:

- **The TFR site is a viewer.** Its documentation ([AboutTFRWebsite.pdf](https://tfr.faa.gov/tfr3/AboutTFRWebsite.pdf)) describes "TFR List in JSON / XML" as a *Download* button for a person, and says: "To verify the latest TFR data, contact NMS (NOTAM Management Service)". It does not offer or address automated access. The site answers our Go client with HTTP 403 (see above).
- **TFRs are FDC NOTAMs, and NMS is the authority.** A May 2021 FAA SWIM workshop Q&A ([media/19481](https://www.faa.gov/media/19481)): asked whether the NOTAM API "contains the actual geo-spatial data for all TFRs", the NOTAM Team answered "Yes, the geometry data is exactly the same that is available via TFR.faa.gov."
- **That is not borne out on NMS staging today.** Checked directly (all against staging, 2026-11-23): 41 of the 47 TFRs on the site's list are retrievable from the NMS-API by number (`accountability=FDC&notamNumber=...`); 6 are not (6/9141, 6/8983, 6/8704, 6/8702, 6/7660, 0/0367). For every TFR compared, the NMS GeoJSON geometry is a bare `Point`, with **no Q-line coordinates and no radius**, and none of the TFR site's published outlines (5 to 88 vertices) is present. The AIXM format has the NOTAM event and text but no airspace geometry either. NMS does carry the full NOTAM text, effective dates and classification for them.
- **Not confirmed:** a SWIM (SCDS) digital-TFR feed. Searches found nothing in FAA documentation; third-party catalogs mention one, unverified. SWIM needs a separate agreement.
- Commercial and community TFR feeds exist; they are not official sources and are not used.

Consequence: today the authorized route (NMS-API) gives TFR NOTAM text and dates but not outlines; the only place outlines are published is the TFR site, whose automated access is blocked. Deriving an area from the NOTAM text would be exactly the kind of logic §0 rules out. Open questions for the FAA are listed in DESIGN.md §7.

### Does Aviation Weather offer TFRs? (2026-11-23)

No, not as data. The documented AWC Data API (`aviationweather.gov/data/schema/openapi.yaml`) has 21 endpoints (`airmet`, `airport`, `airsigmet`, `areafcst`, `cwa`, `dataserver`, `fcstdisc`, `feature`, `fix`, `gairmet`, `isigmet`, `metar`, `mis`, `navaid`, `obstacle`, `pirep`, `sigmet`, `stationinfo`, `taf`, `tcf`, `windtemp`) and the spec never mentions TFRs. The TFR layer on the aviationweather.gov map is a display, and we do not reach into the site's internal map endpoints. The website is still a useful **manual** cross-check of what we return.

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
