# Working on hobbs

An MCP server (Go) that makes official US aviation data available to AI agents: NOAA AWC weather advisories, METARs and TAFs, and FAA NMS-API NOTAMs, all returned by one data tool (`get_flight_briefing_data`), each response wrapped in a provenance envelope. A third tool, `get_briefing_guide`, serves plain-text advice to the agent on briefing a pilot with that data (`internal/mcpserver/guide.md`). Single maintainer, hobby project. Hosted at `https://hobbs.up.railway.app/mcp`.

## Read first

- `docs/DESIGN.md` **§2** (the principles: what this layer must not do), **§4** (the high-level design) and **§9** (the decisions log, with the alternatives already considered). Do not reopen a logged decision, or re-propose a rejected alternative, without the maintainer.
- `docs/DESIGN.md` §3 for the product requirements and user flows, §5.5 for how the upstream services behave, §8 for open items.

## The rules that matter most

- **Docs are part of every change** (core development principle). A change is not done until the docs say what it changed, for two readers:
  - **a person** who wants to understand, at a high level, what the system does and why;
  - **a future development agent** that starts with none of this conversation and must continue seamlessly.

  Update them in the same commit or PR as the change. Write for a reader with no chat history: state facts, decisions and reasons, never "as discussed". Date decisions and observations. When something is superseded, fix or mark it, so no two docs contradict each other. Where each kind of thing goes:
  - `README.md`: what it is, the tools, how to run, host and develop it;
  - `docs/DESIGN.md`: requirements and user flows (§3), the high-level design and its diagram (§4), how each part behaves and why (§5), including what the upstream services were observed to do (§5.5, dated); open items (§8); the decisions log (§9): **major** decisions only (API, scope, architecture, and approaches that did not work), each with its date, its reason and **the alternatives considered and why they lost**, added when the decision is made; process and tooling details belong here in `CLAUDE.md`, not in the log;
  - `CLAUDE.md`: how to work here;
  - `docs/superpowers/specs/` and `docs/superpowers/plans/`: dated design and implementation-plan records for a change, written before it was built. They are history, not kept current: once the change is in, `docs/DESIGN.md` holds what lasts, and where they differ, DESIGN.md wins;
  - the tool descriptions and package comments in the code: what each part does.

  Keep all of these consistent with each other. Draw diagrams in Mermaid (a `mermaid` code block, which GitHub renders), not ASCII art, so they stay readable and editable as text.
- **This layer makes the data available: uniform, consistent, available.** No relevance, ranking, summarizing, route or area intersection, "applies" or "active now" logic. That belongs in higher layers: the server judges no record. The one thing it serves beyond data is advice on *method*, the briefing guide (`get_briefing_guide`, since 2026-11-25); keep that advice in `internal/mcpserver/guide.md`, never in the data path. Ask: if our logic is wrong, does the failure show (extra data, a flag) or hide (a missing record)? Only showing is acceptable.
- **Every data response carries provenance** (sources, retrieval times, every data-pull problem) through `respond` / `provenance.New`; the guide tool's text is advice, not data, and is the one plain-text exception. Never say "none" unless `completeness.status` is `complete`.
- **Filter only with the source's own filters**, and say who applied each one (`applied_by_source` / `applied_by_server`).
- **Promise is as-is**: best-effort copy, no warranty, not characterized as FAA or NWS data.
- **`airworthy` is false** unless started with `-airworthy` and every source is production. The hosted server uses FAA staging, so it is not airworthy.
- Configuration by flag; only secrets come from the environment (`HOBBS_NMS_CLIENT_ID`, `HOBBS_NMS_CLIENT_SECRET`).

## How to work

- **Branches and PRs:** develop on a `claude/...` branch; open a PR to `main` when the maintainer asks; merge only when the maintainer says to, and only once CI is green on the PR's head. Railway deploys `main` automatically on every merge.
- **Railway** (project `hobbs` `de179d84-d4bf-431d-9e95-2d08fdaaf5c2`, service `hobbs` `65ff3c55-512f-435f-b33a-10ae6c354f73`, environment `production` `f09be899-f8e6-4919-b0cc-23a822937cd3`): auto-deploy is on, from `main` (since 2026-11-24, when the Railway GitHub App was given access to the repo; before that no merge deployed). After a merge, check with `list-deployments` that a deployment for the merge commit reaches `SUCCESS`, then run `mcp__hobbs__source_status`. If a merge does not deploy, check the GitHub App's access first; reconnecting the source (`connect-service-source`, `davidhinkes/hobbs`, branch `main`) builds the latest `main` once as a stopgap. "Wait for CI" is on (a dashboard setting: the service → Settings → Source): a merge's deployment sits in `WAITING` until CI on `main` passes, then builds; a red CI run means no deploy.
- **Use the direct Railway MCP tools** (`list-deployments`, `describe-service`, `get-logs`, `connect-service-source`, `update-service`, `search-docs`). Do not use `railway-agent`: it reported a change it had not made, started an unrequested deployment and left an empty staged change. Settings the direct tools cannot reach (auto-deploy, wait for CI, GitHub App access) are the maintainer's to change in the dashboard: ask. The start command is set in Railway settings: `/usr/local/bin/hobbs -http :8080 -nms-url https://api-staging.cgifederal-aim.com`.
- **End-to-end tests run only against the hosted production service**, through the `hobbs` MCP tools that `.mcp.json` registers (`mcp__hobbs__get_flight_briefing_data`, `mcp__hobbs__source_status`, `mcp__hobbs__get_briefing_guide`; the hosted server runs `main`, so a session started before a tool change is merged and deployed still sees the old tools). Do not run the server locally for an end-to-end test (maintainer's instruction). Start with `source_status`. To test a change to the guide: unit tests first; after the merge deploys, start a fresh session, ask for a briefing, and check that the agent called `get_briefing_guide` first and that the briefing is short and keeps the data-quality lines (not airworthy, failed pulls, no TFR outlines). The cloud environment `hobbs` allows `hobbs.up.railway.app`, `aviationweather.gov`, `api-staging.cgifederal-aim.com`, `tfr.faa.gov` and `www.faa.gov`.
- **Unit and integration tests:** `go test ./...` (offline, fixtures in `testdata/`). `go test -tags live ./...` runs the server locally against the live upstream sources and needs the NMS secrets; it is an integration test, not the end-to-end test above. Also run `go vet ./...`, and `staticcheck` and `deadcode` with `GOTOOLCHAIN=go1.25.0` (install them first in a fresh container, one `go install` per tool: `go install honnef.co/go/tools/cmd/staticcheck@latest` and `go install golang.org/x/tools/cmd/deadcode@latest`; run `deadcode ./cmd/hobbs`). `deadcode` reports four expected items, kept by the maintainer's choice: `model.TFR.Validate` (the TFR adapter is off), `source.NotEnabled`, and `nms.Disabled.ID` / `.Fetch`; they are listed in `.github/deadcode-expected.txt`, and CI fails on anything else.
- **CI** (`.github/workflows/ci.yml`) runs on every pull request and every push to `main`: gofmt, `go vet`, build, `go test ./...`, `staticcheck` and `deadcode` (pinned versions), and a Docker build of the image Railway deploys. Keep it green; when a check fails, fix the cause, never skip or weaken the check. If a kept item changes, update `.github/deadcode-expected.txt` deliberately.

## Do not

- Commit secrets, or print FAA credentials or copy them into tool calls or chat. They live in Railway service variables; any local copy is for one session only.
- Work around access controls: `tfr.faa.gov` has blocked this client (HTTP 403) and we do not disguise the client. The TFR adapter stays turned off.
- Email the FAA for help (maintainer's decision), or send the maintainer's personal details to outside services.
- Add route input or infer en-route centers between airports (DESIGN.md §3.4, §9).
