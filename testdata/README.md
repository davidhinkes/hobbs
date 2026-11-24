# Test data

Saved copies of real responses, captured by hand on 2026-11-22. Tests serve these files to our code from a local fake server, so they need no internet and give the same result every time. None of this is part of the server binary.

| File | Where it came from | What was kept |
|---|---|---|
| `awc/airsigmet.json`, `awc/gairmet.json`, `awc/cwa.json` | aviationweather.gov, `/api/data/<name>?format=json` | the whole response |
| `awc/isigmet.json` | aviationweather.gov, `/api/data/isigmet?format=json` | the first 10 of 158 records |
| `awc/isigmet_multipart.json` | same feed | 2 records (NZKL 22, SBGL 39) that cross the antimeridian |
| `awc/metar.json` | aviationweather.gov, `/api/data/metar?ids=KSMO,KSNA,KLAX&format=json`, captured 2026-11-24 | the whole response (3 METARs), plus two hand-added records: a `SPECI` copy of KSMO (new `metarType`, `obsTime`, `rawOb`, `fltCat`) and a record `KBAD` with `"obsTime":"garbage"` and no `rawOb` |
| `awc/taf.json` | aviationweather.gov, `/api/data/taf?ids=KSNA,KSMO&format=json`, captured 2026-11-24 | the whole response, reformatted with indentation |
| `nms/notams_sample.json` | FAA NMS-API **staging**, GeoJSON, queries for KIAD and 25 nm around it | 16 NOTAMs picked to cover odd cases (permanent, cancelled, schedules, odd altitudes, multi-part areas) |
| `tfr/list.json` | tfr.faa.gov TFR list | the whole response |
| `tfr/detail_*.xml` | tfr.faa.gov TFR detail documents | 5 TFRs, one per kind of shape (text-only, circle, polygon, multi-area, arc) |

The NOTAM data is from a test environment and may include test entries. All of it is provided as is: a copy, not FAA or NWS data as published, and not endorsed by either.
