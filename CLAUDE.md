# Working on hobbs

An MCP server (Go) that makes official US aviation data available to AI agents: NOAA AWC weather advisories and FAA NMS-API NOTAMs, each response wrapped in a provenance envelope. Single maintainer, hobby project. Hosted at `https://hobbs.up.railway.app/mcp`.

## Read first

- `docs/DESIGN.md` **§0** (the mantra and what this layer must not do) and **§8** (the decisions log). Do not reopen a logged decision without the maintainer.
- `docs/DESIGN.md` §7 for open items; `docs/SPIKE.md` for what the upstream services were observed to do.

## The rules that matter most

- **This layer makes the data available: uniform, consistent, available.** No relevance, ranking, summarizing, route or area intersection, "applies" or "active now" logic. That belongs in higher layers. Ask: if our logic is wrong, does the failure show (extra data, a flag) or hide (a missing record)? Only showing is acceptable.
- **Every response carries provenance** (sources, retrieval times, every data-pull problem) through `respond` / `provenance.New`. Never say "none" unless `completeness.status` is `complete`.
- **Filter only with the source's own filters**, and say who applied each one (`applied_by_source` / `applied_by_server`).
- **Promise is as-is**: best-effort copy, no warranty, not characterized as FAA or NWS data.
- **`airworthy` is false** unless started with `-airworthy` and every source is production. The hosted server uses FAA staging, so it is not airworthy.
- Configuration by flag; only secrets come from the environment (`HOBBS_NMS_CLIENT_ID`, `HOBBS_NMS_CLIENT_SECRET`).

## How to work

- **Branches and PRs:** develop on a `claude/...` branch; open a PR to `main` when the maintainer asks; merge only when the maintainer says to.
- **Railway** (project `hobbs` `de179d84-d4bf-431d-9e95-2d08fdaaf5c2`, service `hobbs` `65ff3c55-512f-435f-b33a-10ae6c354f73`, environment `production` `f09be899-f8e6-4919-b0cc-23a822937cd3`): **a merge does not deploy by itself.** Auto-deploy on push has not been seen to work: every deployment so far (through PR #4) started when the source was connected, and the service config shows no branch afterwards. So after each merge, connect the source again (`connect-service-source`, repo `davidhinkes/hobbs`, branch `main`), which builds the latest `main` at once; then check with `list-deployments` that a deployment for the merge commit reaches `SUCCESS`. Why auto-deploy does not fire is unknown (open item in DESIGN.md §7). The start command is set in Railway settings: `/usr/local/bin/hobbs -http :8080 -nms-url https://api-staging.cgifederal-aim.com`.
- **End-to-end tests run only against the hosted production service**, through the `hobbs` MCP tools that `.mcp.json` registers (`mcp__hobbs__get_notams`, `mcp__hobbs__get_weather_advisories`, `mcp__hobbs__source_status`). Do not run the server locally for an end-to-end test (maintainer's instruction). Start with `source_status`. The cloud environment `hobbs` allows `hobbs.up.railway.app`, `aviationweather.gov`, `api-staging.cgifederal-aim.com`, `tfr.faa.gov` and `www.faa.gov`.
- **Unit and integration tests:** `go test ./...` (offline, fixtures in `testdata/`). `go test -tags live ./...` runs the server locally against the live upstream sources and needs the NMS secrets; it is an integration test, not the end-to-end test above. Also run `go vet ./...`, and `staticcheck` and `deadcode` with `GOTOOLCHAIN=go1.25.0` (install them first in a fresh container, one `go install` per tool: `go install honnef.co/go/tools/cmd/staticcheck@latest` and `go install golang.org/x/tools/cmd/deadcode@latest`; run `deadcode ./cmd/hobbs`). `deadcode` reports four expected items, kept by the maintainer's choice: `model.TFR.Validate` (the TFR adapter is off), `source.NotEnabled`, and `nms.Disabled.ID` / `.Fetch`. Anything else it reports is new.
- **Docs:** record observations of upstream behavior in `docs/SPIKE.md` (dated), and decisions in `docs/DESIGN.md` §8 (dated, with the reason). Keep README, DESIGN and the tool descriptions in the code consistent with each other.

## Do not

- Commit secrets, or print FAA credentials or copy them into tool calls or chat. They live in Railway service variables; any local copy is for one session only.
- Work around access controls: `tfr.faa.gov` has blocked this client (HTTP 403) and we do not disguise the client. The TFR adapter stays turned off.
- Email the FAA for help (maintainer's decision), or send the maintainer's personal details to outside services.
- Add route input or infer en-route centers between airports (DESIGN.md §5, §8).
