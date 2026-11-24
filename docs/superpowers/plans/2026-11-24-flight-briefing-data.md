# get_flight_briefing_data Implementation Plan

**Status: executed, 2026-11-24** (commits `04edcbf`..`ec943ae` on `claude/nifty-einstein-2wh075`; the checkboxes below were not ticked). Kept as a dated record and not updated further: `docs/DESIGN.md` is the source of truth, and where they differ, DESIGN.md wins.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One MCP tool, `get_flight_briefing_data`, that takes ICAO airports (plus optional en-route centers) and returns NOTAMs, METARs, TAFs and the four AWC advisory sets in one provenance envelope, replacing `get_notams` and `get_weather_advisories`.

**Architecture:** A new AWC adapter path fetches METARs and TAFs by station (`ids=`) into a new `model.StationReport`. The NOTAM and advisory handlers' bodies are first extracted into reusable collectors; the new tool validates its input, builds a translation (what each source is asked), runs the collectors in parallel and returns one envelope through `respond`. Then the two old tools, their tests and their now-dead helpers are removed, and the docs are updated.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk/mcp`, standard library only. Tests offline with `httptest` and fixtures in `testdata/`.

**Spec:** `docs/superpowers/specs/2026-11-24-flight-briefing-data-design.md` (approved 2026-11-24; time window dropped, §6).

## Global Constraints

- `docs/DESIGN.md` §2: no relevance, "applies", "in effect now" or flight-category logic; no server-side filtering beyond what the source applies; nothing dropped without a flag.
- Every response goes through `respond` (`internal/mcpserver/server.go`) and `provenance.New`; no bare data.
- Airports: exactly 4 letters or digits after trimming and upper-casing; 1 to 10 per call; duplicates removed. Centers: `^K?Z[A-Z]{2}$` after upper-casing, `KZLA` and `ZLA` the same. Airports plus centers at most 10 (the existing `maxLocations`, the FAA request budget).
- A 3-letter airport (`SMO`) is an input error whose message names the ICAO form (`KSMO`); the server never adds a `K`. Selectors such as `@CA` are input errors.
- `include` values, in this output order: `notams`, `metars`, `tafs`, `sigmets`, `airmets`, `intl_sigmets`, `cwas`. Default all seven.
- `include_geometry` defaults to **false** in this tool; when false, `omitted_fields` lists `geometry`.
- No time window, no `limit`, no `hazard`, no point/radius, no other NMS filters.
- New `raw_format` values: `metar` (METAR and SPECI) and `taf`.
- New issue code `STATION_NO_DATA` (warning). Its message must say AWC answers the same way for "no current report", "no TAF published here" and "unknown station".
- Provenance source IDs: `awc.metar`, `awc.taf` (new); `faa.nms.notam:<LOC>`, `awc.airsigmet`, `awc.gairmet`, `awc.isigmet`, `awc.cwa` (existing).
- The tool description starts with the not-airworthy text (via `d.describe`), says it returns METARs and TAFs as published by AWC, that `fltCat` in a report's `details` is AWC's flight category, not computed by hobbs, that centers between airports are never inferred, that advisories are the full national sets, and ends with `reliance`.
- AWC field facts (probed 2026-11-24): METAR empty = HTTP 204 + empty body; TAF empty = HTTP 200 + `[]`; times mix epoch seconds (`obsTime`, `validTimeFrom`, `validTimeTo`) and ISO strings (`reportTime`, `receiptTime`, `issueTime`); `elev` is metres; `lat`/`lon` decimal degrees.
- Checks that must pass before every commit: `gofmt -l .` prints nothing, `go vet ./...`, `go test ./...`. Before the last commit also `GOTOOLCHAIN=go1.25.0 staticcheck ./...` and `GOTOOLCHAIN=go1.25.0 deadcode ./cmd/hobbs` (only the four items in `.github/deadcode-expected.txt`).
- Commit messages end with the two attribution lines from the session (`Co-Authored-By: ...`, `Claude-Session: ...`). Work on branch `claude/nifty-einstein-2wh075`.

## Review Focus

1. **The same airport twice in different case or with spaces** (`["ksmo", " KSMO"]`): one FAA query, one `ids` entry, `request.airports` is `["KSMO"]`. Pinned in Task 4 (`TestBriefingNormalizesAirports`).
2. **A center written as an airport** (`airports: ["KZLA"]`, which is also a valid-looking ICAO code; real airports such as KZPH start with KZ, so it cannot be rejected): NOTAMs are fetched for it, and the METAR and TAF sublists carry `STATION_NO_DATA` naming `KZLA`, never a silent gap. Pinned in Task 4 (`TestBriefingCenterInAirportsIsFlaggedNotHidden`).
3. **AWC returns a station that was not asked for, or omits one that was**: the extra one is kept (nothing dropped), the missing one is in `STATION_NO_DATA` refs. Pinned in Task 2 (`TestStationReportsMissingAndExtraStations`).
4. **NOTAMs not enabled on the server** (no `-nms-url`) with the default `include`: the weather sublists are still returned, the response is `incomplete` with `SOURCE_NOT_ENABLED`, and `isError` is false. Pinned in Task 4 (`TestBriefingWithoutNOTAMSourceStillReturnsWeather`).
5. **`include: []` or an unknown value** (`"pireps"`): an input error listing the valid values, never an empty briefing. Pinned in Task 4 (`TestBriefingInputErrors`).

---

### Task 1: The station report model

**Files:**
- Create: `internal/model/station.go`
- Modify: `internal/model/text.go` (two `RawFormat` constants)
- Test: `internal/model/station_test.go`

**Interfaces:**
- Produces:
  - `model.RawMETAR RawFormat = "metar"`, `model.RawTAF RawFormat = "taf"`
  - `type ReportKind string`; `KindMETAR ReportKind = "METAR"`, `KindTAF ReportKind = "TAF"`
  - `type StationReport struct` with JSON fields, in this order: `id`, `kind`, `source`, `report_type,omitempty`, `station`, `station_name,omitempty`, `elevation_m,omitempty` (`*float64`), `geometry,omitempty` (`*Geometry`), `observed_at,omitempty`, `reported_at,omitempty`, `received_at,omitempty`, `issued_at,omitempty`, `valid_from,omitempty`, `valid_to,omitempty` (all `*time.Time`), `details,omitempty` (`json.RawMessage`), `raw_text,omitempty`, `raw_format,omitempty`, `source_record,omitempty` (`json.RawMessage`), `parse_issues,omitempty` (`[]string`). Go names: `ID, Kind, Source, ReportType, Station, StationName, ElevationM, Geometry, ObservedAt, ReportedAt, ReceivedAt, IssuedAt, ValidFrom, ValidTo, Details, RawText, RawFormat, SourceRecord, ParseIssues`.
  - `func (r StationReport) Validate() error`

- [ ] **Step 1: Write the failing test** `TestStationReportValidate`, table-driven, in `internal/model/station_test.go`:
  - valid METAR (`ID "METAR KSNA 2026-11-24T05:53:00Z"`, `Kind KindMETAR`, `Source "awc.metar"`, `Station "KSNA"`, `RawText "METAR KSNA 240553Z ..."`, `RawFormat RawMETAR`, `Geometry` = `NewPoint(-117.86745, 33.67976)`) → nil
  - valid TAF with `RawFormat RawTAF`, `ValidFrom` before `ValidTo` → nil
  - missing `ID`, missing `Kind`, missing `Source`, missing `Station` → error each
  - METAR with `RawFormat RawTAF` → error; TAF with `RawFormat RawMETAR` → error
  - `ValidTo` before `ValidFrom` → error
  - no `RawText` and no `SourceRecord` → error; no `RawText` with `SourceRecord` `{"icaoId":"KSNA"}` → nil
  - `Geometry` with latitude 95 → error
- [ ] **Step 2: Run** `go test ./internal/model -run TestStationReportValidate`. Expected: FAIL (undefined `StationReport`).
- [ ] **Step 3: Implement** `StationReport`, `ReportKind`, the two `RawFormat` constants (with doc comments like the existing ones) and `Validate`, which uses `validateText("station report", r.ID, r.RawText, r.RawFormat, []RawFormat{want}, r.SourceRecord)` where `want` is `RawMETAR` for `KindMETAR` and `RawTAF` for `KindTAF`, and `r.Geometry.Validate()` when set. Package comment on the type: AWC's decoded fields are kept as published in `Details`; hobbs computes nothing from them.
- [ ] **Step 4: Run** `go test ./internal/model`. Expected: PASS.
- [ ] **Step 5: Commit** `internal/model/station.go internal/model/station_test.go internal/model/text.go` — "Add the station report model for METARs and TAFs".

### Task 2: AWC METAR and TAF fetch

**Files:**
- Create: `internal/source/awc/stations.go`, `testdata/awc/metar.json`, `testdata/awc/taf.json`
- Modify: `internal/source/awc/awc.go` (extract the HTTP fetch; package comment), `internal/provenance/provenance.go` (`StationNoData`), `testdata/README.md` (the two fixtures)
- Test: `internal/source/awc/stations_test.go`

**Interfaces:**
- Consumes: Task 1 `model.StationReport`, `KindMETAR`, `KindTAF`, `RawMETAR`, `RawTAF`.
- Produces:
  - `provenance.StationNoData Code = "STATION_NO_DATA"`, registered in `knownCodes`
  - `awc.IDMETAR = "awc.metar"`, `awc.IDTAF = "awc.taf"`
  - `func (c *Client) METARs(ctx context.Context, stations []string) (source.Snapshot[model.StationReport], error)`
  - `func (c *Client) TAFs(ctx context.Context, stations []string) (source.Snapshot[model.StationReport], error)`
  - Both request `<BaseURL>/metar?ids=KSMO,KSNA&format=json` (resp. `/taf`), `ids` joined with commas in the given order; the error wraps `ErrUpstream`, and on error the snapshot's `Source` is filled as far as the fetch got (as `feed.Fetch` does).
  - Unexported `func (c *Client) getJSON(ctx context.Context, id, url string) (provenance.Source, []json.RawMessage, error)`: the request, size cap, hash, ETag and status rules from `feed.Fetch`, where HTTP 204 with an empty body and HTTP 200 with `[]` both mean zero records; 200 with an empty body, non-2xx and non-array bodies are errors. `feed.Fetch` is changed to use it, with no change in behavior.

- [ ] **Step 1: Capture fixtures.** `testdata/awc/metar.json`: the live response of `https://aviationweather.gov/api/data/metar?ids=KSMO,KSNA,KLAX&format=json`, then hand-edit to add one `SPECI` record (`"metarType":"SPECI"`) and one record with `"obsTime":"garbage"` and no `rawOb`. `testdata/awc/taf.json`: the live response for `ids=KSNA,KSMO`. Note both in `testdata/README.md` with the date and the edits.
- [ ] **Step 2: Write the failing tests** in `stations_test.go` (reuse `serve`, `fixedNow` from `awc_test.go`; add a `serveCapture` helper that records the request URL):
  - `TestMETARFixture`: `METARs(ctx, []string{"KSMO","KSNA","KLAX"})` against `metar.json`; the request query is `ids=KSMO,KSNA,KLAX&format=json`; the KLAX report has `Kind KindMETAR`, `ReportType "METAR"`, `Station "KLAX"`, `ObservedAt` = `time.Unix(<obsTime from fixture>,0).UTC()`, `RawFormat RawMETAR`, `RawText` equal to the fixture's `rawOb`, `Geometry` a point at the fixture's `lon`/`lat`, `ElevationM` the fixture's `elev`, `ID` `"METAR KLAX " + ObservedAt.Format(time.RFC3339)`; `Details` contains `"fltCat"` and does not contain `"rawOb"`; the SPECI record has `ReportType "SPECI"`; the bad record is present, has `ParseIssues` and `SourceRecord`, and is listed in a `PARSE_FAILED` issue; `snap.Source.ID == "awc.metar"`, `RecordsInFeed` = fixture length.
  - `TestTAFFixture`: `ValidFrom`/`ValidTo` from epoch seconds, `IssuedAt` from the ISO string, `RawFormat RawTAF`, `Details` contains `"fcsts"`, `ID` `"TAF KSNA " + IssuedAt.Format(time.RFC3339)`.
  - `TestStationEmptyAnswersAreNotFailures`: METAR 204 + empty body, and TAF 200 + `[]`, each return no error, zero items, and a `STATION_NO_DATA` warning with `Refs` equal to the requested stations.
  - `TestStationReportsMissingAndExtraStations`: request `["KSNA","KXYZ"]` against a body holding KSNA and KLAX: both KSNA and KLAX are returned (nothing dropped), and `STATION_NO_DATA` has `Refs ["KXYZ"]` and the message text from Global Constraints.
  - `TestStationFailuresAreErrors`: 500, 200 with empty body, and `{"not":"an array"}` each return an error wrapping `ErrUpstream` and a snapshot whose `Source.HTTPStatus` is set.
- [ ] **Step 3: Run** `go test ./internal/source/awc`. Expected: the new tests FAIL (undefined `METARs`); the existing ones PASS.
- [ ] **Step 4: Implement.** Extract `getJSON` from `feed.Fetch` and use it there. In `stations.go`, one unexported `fetchStations(ctx, id, path string, kind model.ReportKind, stations []string)` behind `METARs` and `TAFs`, and two record parsers using the existing `flexTime`, `flexFloat`, `flexString` types: METAR maps `icaoId, name, elev, lat, lon, metarType, obsTime, reportTime, receiptTime, rawOb`; TAF maps `icaoId, name, elev, lat, lon, issueTime, validTimeFrom, validTimeTo, rawTAF`. `Details` is the record with exactly those mapped keys removed, re-marshalled. A record with no `icaoId` becomes a stub (`ID "<source>#<index>"`, `SourceRecord` set) as `feed.Fetch` does; repeated IDs get `" #2"`, `" #3"`. Issues: `PARSE_FAILED` (warning) via `source.AppendIssue`, then `STATION_NO_DATA` (warning, `Refs` = requested stations, upper-cased, with no record in the result, in request order) when any. Update the package comment: station feeds are filtered by AWC with `ids`; the advisory feeds are fetched whole.
- [ ] **Step 5: Run** `go test ./internal/source/awc ./internal/provenance`. Expected: PASS.
- [ ] **Step 6: Commit** — "Fetch METARs and TAFs from AWC by station".

### Task 3: Extract the NOTAM and advisory collectors

**Files:**
- Modify: `internal/mcpserver/notams.go`, `internal/mcpserver/weather.go`
- Test: existing `internal/mcpserver/server_test.go` (unchanged; it must stay green)

**Interfaces:**
- Produces:
  - `type notamResult struct { NOTAMs []model.Notam; Locations *locationsInfo; Applied map[string]string; Sources []provenance.Source; Issues []provenance.Issue; NotConsulted []provenance.NotConsulted; Failed, Queries int }`
  - `func collectNotams(ctx context.Context, d Deps, base nms.Query, locs []string, addCenters bool) (notamResult, error)` — everything `notamsHandler` does from the `d.NMS == nil` check through the merge and per-source `RecordsReturned`, except the caller's `limit`/`include_geometry` shaping. With `d.NMS == nil` it returns the `not_enabled` not-consulted entry and the `SOURCE_NOT_ENABLED` critical issue, `Queries` 0. The error is non-nil only for `nms.ErrBadQuery`.
  - `type advisoryResult struct { ByKind map[string][]model.Advisory; Sources []provenance.Source; Issues []provenance.Issue; Failed, Queries int }`
  - `func collectAdvisories(ctx context.Context, d Deps, feeds []weatherFeed) advisoryResult` — the parallel fetch and failure handling of `weatherHandler`, keyed by `weatherFeed.kind`, records in source order.
  - `notamsHandler` and `weatherHandler` are rewritten on top of these, keeping their output byte-for-byte (they are deleted in Task 5).

- [ ] **Step 1: Run** `go test ./internal/mcpserver` and note it passes before the change.
- [ ] **Step 2: Refactor** as above. `RecordsReturned` is still set after shaping by the handlers, so `collectNotams` sets it from the merged result and the handlers recompute it after `limit`.
- [ ] **Step 3: Run** `go test ./internal/mcpserver`. Expected: PASS, with no test edited.
- [ ] **Step 4: Commit** — "Extract NOTAM and advisory collectors from the tool handlers".

### Task 4: The `get_flight_briefing_data` tool

**Files:**
- Create: `internal/mcpserver/briefing.go`
- Modify: `internal/mcpserver/server.go` (register it), `internal/mcpserver/server_test.go` (fake AWC serves `/metar` and `/taf` from the new fixtures and records their query strings in a new `awcQueries map[string]string` field)
- Test: `internal/mcpserver/briefing_test.go`

**Interfaces:**
- Consumes: Task 2 `(*awc.Client).METARs`, `TAFs`, `awc.IDMETAR`, `awc.IDTAF`; Task 3 `collectNotams`, `collectAdvisories`, `weatherFeeds`, `centerKey`, `centersNotInferred`, `maxLocations`.
- Produces:
  - `type BriefingInput struct { Airports []string \`json:"airports"\`; Centers []string \`json:"centers,omitempty"\`; Include []string \`json:"include,omitempty"\`; IncludeGeometry *bool \`json:"include_geometry,omitempty"\` }` with `jsonschema` descriptions stating the Global Constraints rules.
  - `type briefingRequest struct { Airports, Centers, Include []string; IncludeGeometry bool }` (JSON `airports`, `centers`, `include`, `include_geometry`): the input as understood.
  - `type translationEntry struct { Source string \`json:"source"\`; AppliedBySource map[string]string \`json:"applied_by_source"\`; NotUsed []string \`json:"not_used"\` }`
  - `func translate(req briefingRequest) []translationEntry` — pure; one entry per included source group, in `include` order: `faa.nms.notam` (`location` = airports then centers, comma-joined; `NotUsed` empty; implied centers are reported in `locations.added`, not here), `awc.metar` and `awc.taf` (`ids` = airports; `NotUsed` = `["centers"]` when centers were given), each advisory feed id (`AppliedBySource` empty; `NotUsed` = `["airports", "centers"]`, meaning the national set is not filtered by place).
  - `func validateBriefing(in BriefingInput) (briefingRequest, error)` — the Global Constraints rules; error messages: `"airports: SMO is not a 4-character ICAO identifier; use the ICAO form, e.g. KSMO"`, `"centers: ... is not an en-route center identifier such as ZLA"`, `"include: ... is not one of notams, metars, tafs, sigmets, airmets, intl_sigmets, cwas"`, `"include must not be empty; omit it for everything"`, `"at most 10 airports and centers per call (got N); split the request"`, `"airports is required"`.
  - `type briefingData struct` with JSON fields `request`, `translation`, `locations,omitempty` (`*locationsInfo`), `omitted_fields`, then `notams`, `metars`, `tafs`, `sigmets`, `airmets`, `intl_sigmets`, `cwas`, each `*[]T,omitempty` so a sublist not included is absent and an included empty one is `[]`.
  - Tool name `get_flight_briefing_data`, annotations `readOnly("NOTAMs, METARs, TAFs and weather advisories for a flight")`.

- [ ] **Step 1: Write the failing tests** in `briefing_test.go`:
  - `TestTranslateIsShownPerSource`: `translate` for airports `["KSMO","KSNA"]`, centers `["ZAB"]`, all include → seven entries in order `faa.nms.notam` (`location "KSMO,KSNA,ZAB"`), `awc.metar` (`ids "KSMO,KSNA"`, `NotUsed ["centers"]`), `awc.taf` (same), `awc.airsigmet`, `awc.gairmet`, `awc.isigmet`, `awc.cwa` (each `NotUsed ["airports","centers"]`).
  - `TestBriefingReturnsEverySublistWithOneEnvelope`: call with `airports ["KSMO","KSNA"]`; not `isError`; data has all seven sublist keys; `omitted_fields` is `["geometry"]` and no NOTAM or advisory has `geometry`; provenance sources include `faa.nms.notam:KSMO`, `faa.nms.notam:KSNA`, the implied center from the NMS fixture, `awc.metar`, `awc.taf` and the four advisory feeds; `locations.note` equals `centersNotInferred`; the fake AWC saw `/metar` with `ids=KSMO,KSNA`.
  - `TestBriefingIncludeSelectsSublists`: `include ["tafs","sigmets"]` → only `tafs` and `sigmets` keys in data, only `awc.taf` and `awc.airsigmet` in provenance, no NMS query made.
  - `TestBriefingGeometryOnRequest`: `include_geometry true` → `omitted_fields` empty and advisories carry geometry.
  - `TestBriefingNormalizesAirports` (Review Focus 1): `["ksmo"," KSMO","KSNA"]` → `request.airports ["KSMO","KSNA"]`, exactly two FAA location queries for airports, `ids=KSMO,KSNA`.
  - `TestBriefingCenterInAirportsIsFlaggedNotHidden` (Review Focus 2): `["KZLA"]` → an FAA query for `KZLA`; a `STATION_NO_DATA` issue for source `awc.metar` with `KZLA` in refs, and one for `awc.taf`.
  - `TestBriefingCentersGoToNOTAMsOnly`: `centers ["kzab"]` → an FAA query for `ZAB`; `/metar` ids do not contain it; `request.centers ["ZAB"]`.
  - `TestBriefingWithoutNOTAMSourceStillReturnsWeather` (Review Focus 4): deps without NMS → not `isError`, status `incomplete`, `SOURCE_NOT_ENABLED` present, `metars` non-empty.
  - `TestBriefingOneFailedSourceIsVisible`: fake AWC returns 503 for `/taf` → not `isError`, status `incomplete`, `SOURCE_UNAVAILABLE` for `awc.taf`, `metars` still non-empty, `tafs` is `[]`.
  - `TestBriefingAllSourcesFailedIsAnError`: NMS and every AWC path fail → `isError` true and the envelope is present.
  - `TestBriefingInputErrors` (Review Focus 5): each of `airports ["SMO"]`, `["@CA"]`, `[]`, 11 airports, 8 airports + 3 centers, `centers ["LAX"]`, `include []`, `include ["pireps"]` → `isError` with the matching message from Interfaces, and no upstream request was made.
- [ ] **Step 2: Run** `go test ./internal/mcpserver -run 'Briefing|Translate'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `briefing.go`: `validateBriefing` → `translate` → run `collectNotams` (with `base` = empty `nms.Query`, `locs` = airports then centers, `addCenters` true), `METARs`, `TAFs` and `collectAdvisories` (only the included ones) in parallel via `parallel`; assemble `briefingData`; strip geometry unless requested; `isError` only when every queried source failed (`Failed == Queries` summed, with NOTAMs-not-enabled counting as failed when NOTAMs were included); `respond(d, data, now, sources, notConsulted, issues, isErr)`. Register in `New` after the existing tools. Description per Global Constraints.
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS (old tools still present and green).
- [ ] **Step 5: Commit** — "Add get_flight_briefing_data: NOTAMs, METARs, TAFs and advisories in one call".

### Task 5: Remove `get_notams` and `get_weather_advisories`; METAR/TAF in `source_status`

**Files:**
- Modify: `internal/mcpserver/notams.go` (keep only `collectNotams`, `run`, `cleanLocations` if still used, `centerKey`, `impliedCenters`, `notamKey`, the types and constants they need), `internal/mcpserver/weather.go` (keep `weatherFeed`, `weatherFeeds`, `collectAdvisories`), `internal/mcpserver/server.go` (registration; server instructions name the new tool and METAR/TAF; `reliance` lists `raw_format` values `icao_notam, us_notam, advisory_text, metar or taf`; delete `checkLimit`, `maxLimit`, `truncationIssue`, `parseTime` and `Filters`/`newFilters` if unused), `internal/mcpserver/status.go`, `internal/mcpserver/server_test.go`, `internal/mcpserver/http_test.go`, `internal/mcpserver/live_test.go`
- Test: the same files

**Interfaces:**
- Consumes: Task 4's tool; Task 2's `METARs`/`TAFs`.
- Produces: `source_status` reports `awc.metar` and `awc.taf` (probe station `KLAX`, one request each) after the four advisory feeds and before NMS.

- [ ] **Step 1: Update tests first.**
  - `server_test.go`: delete the tests that exist only for removed inputs (`TestWeatherKindsAndHazardFilters`, `TestNotamsForwardFiltersAndSayWhoAppliedThem`, `TestNotamRequestErrorsAreCorrectableByTheAgent`, `TestCallerChosenLimitIsReportedNotSilent`, `TestNotamCentersAreOnlyAddedWhenWantedAndNew`); rewrite the others to call `get_flight_briefing_data` with the same assertions on its sublists (`TestWeatherReturnsEverythingWithProvenance`, `TestWeatherFailuresAreVisibleNotSilent`, `TestNotamsWhenTheSourceIsUnavailable`, `TestGeometryCanBeOmittedOnRequestAndSaysSo`, `TestAirworthyIsOnEveryResponse`, `TestNotamLocationsAreMergedAndTheirCenterAdded`, `TestNotamCenterThatCannotBeImpliedIsAWarning`, `TestOneFailedLocationMakesTheResponseIncompleteNotEmpty`, `TestNotamLocationsAreValidated` → ICAO rules). `TestToolsAreListedWithTheContractInTheirDescriptions` expects exactly `get_flight_briefing_data` and `source_status`. `TestSharedParametersAreDescribedTheSame` is deleted (one data tool). `TestSourceStatus` expects `awc.metar` and `awc.taf`.
  - `http_test.go` `TestEndToEndOverHTTP`: call `get_flight_briefing_data` with `airports ["KSMO"]`, `include ["cwas"]`.
  - `live_test.go`: replace the old calls with `get_flight_briefing_data` for `["KSMO","KEMT"]`, the same with `include_geometry true`, `["KSMO"]` with `include ["metars","tafs"]`, and `["SMO"]` (a correctable error).
- [ ] **Step 2: Run** `go test ./internal/mcpserver`. Expected: FAIL (old tools still registered; status lacks METAR/TAF).
- [ ] **Step 3: Remove** the two registrations, handlers and input types; add the two `source_status` checks; update the server instructions and `reliance`; delete helpers that are now unused.
- [ ] **Step 4: Run** `gofmt -l .`, `go vet ./...`, `go test ./...`, `GOTOOLCHAIN=go1.25.0 staticcheck ./...`, `GOTOOLCHAIN=go1.25.0 deadcode ./cmd/hobbs`. Expected: no gofmt output; tests PASS; staticcheck clean; deadcode prints exactly the four lines of `.github/deadcode-expected.txt` (delete any other dead function it reports rather than adding it to the list).
- [ ] **Step 5: Run** `go test -tags live ./internal/source/awc` (AWC needs no secrets). Expected: PASS. Skip `./internal/mcpserver` live if NMS secrets are absent and say so in the commit message.
- [ ] **Step 6: Commit** — "Replace get_notams and get_weather_advisories with get_flight_briefing_data".

### Task 6: Docs

**Files:**
- Modify: `docs/DESIGN.md`, `README.md`, `CLAUDE.md`, `docs/superpowers/specs/2026-11-24-flight-briefing-data-design.md` (status line: implemented, with the commit; DESIGN.md is now the source of truth)

- [ ] **Step 1: `docs/DESIGN.md`**, every item in spec §8, as facts for a reader with no history, dated 2026-11-24:
  - §1 status and purpose (METARs/TAFs added; one data tool).
  - §3.2: F1 becomes "Data for a flight" with `get_flight_briefing_data` (airports, centers, include); F2 (point and radius) and F3 (advisories alone) removed; F4/F5/F6 renumbered and updated to the tool names.
  - §3.4: no time window (FAA window is containment), bbox deferred, the narrower NOTAM and advisory queries dropped.
  - §4: diagram node text and the request walk-through for the new tool (translation, collectors in parallel, one envelope).
  - §5.1: `raw_format` values `metar` and `taf`; `STATION_NO_DATA` row; `CENTER_NOT_IMPLIED` row names the new tool; `RESULT_TRUNCATED` marked "not emitted today: no tool takes `limit`".
  - §5.2: two tools; the new tool's input, translation table and output (from spec §5.1 to §5.3); `include_geometry` default false here.
  - §5.3: the station report.
  - §5.5: AWC METAR/TAF observations (spec §7) and the FAA effective-window probe (spec §6).
  - §5.7: code layout (`internal/source/awc` station feeds, `internal/model` station report).
  - §6: tests (the live test calls).
  - §8: open items: bbox for METAR/TAF; the narrower queries if wanted again; response size note updated.
  - §9: new rows — one data tool replacing the two (alternatives from spec §4.2); ICAO-only airports plus optional centers (spec §4.3); **did not work: time window** (spec §6 probe, alternatives from spec §4.3); METAR/TAF from AWC (spec §4.1); record content as published (spec §5.4); `include_geometry` default false and `limit` dropped. Mark the 2026-11-23 "a flight is a list of identifiers" row as superseded by the new row (its center rule is kept).
- [ ] **Step 2: `README.md`**: the tools table (two tools), the example call, the TFR sentence names the new tool.
- [ ] **Step 3: `CLAUDE.md`**: the end-to-end tools list (`mcp__hobbs__get_flight_briefing_data`, `mcp__hobbs__source_status`).
- [ ] **Step 4: Check consistency:** `grep -rn "get_notams\|get_weather_advisories" --include=*.md --include=*.go .` shows only historical mentions in DESIGN.md §9 and the spec.
- [ ] **Step 5: Commit** — "Document get_flight_briefing_data, METARs and TAFs".

### Task 7: Push and hand over

- [ ] **Step 1:** `git push -u origin claude/nifty-einstein-2wh075` (retry up to 4 times with backoff on network errors).
- [ ] **Step 2:** Tell the maintainer the branch is ready, with the test results. Open a PR to `main` only when the maintainer asks (`CLAUDE.md`). After a merge and deploy: `list-deployments` shows `SUCCESS` for the merge commit, then `mcp__hobbs__source_status`, then `mcp__hobbs__get_flight_briefing_data` with `airports ["KSMO","KSNA"]` as the end-to-end test.
