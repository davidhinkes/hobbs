# Briefing Guide (`get_briefing_guide`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server serves a short guide that tells an agent how to brief a general aviation pilot with hobbs's data, through a third tool the agent is told to call first.

**Architecture:** The guide is a Markdown file embedded in the binary and returned as plain text by a new no-input tool, `get_briefing_guide`. One sentence in the server instructions and one in the data tool's description point the agent at it. The data path (`get_flight_briefing_data`, the envelope, the sources) does not change.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk` v1.8.0, `embed`.

**Spec:** `docs/superpowers/specs/2026-11-25-briefing-guide-design.md`

## Global Constraints

- Read `CLAUDE.md` and `docs/DESIGN.md` §2 first. No change to `get_flight_briefing_data`'s input, output or behavior; only one sentence is added to its description.
- The guide text is the spec's §5.3 draft, copied exactly. Do not reword it in this plan's tasks.
- The pointer sentence, exactly: `Before briefing a pilot, call get_briefing_guide once and follow it.`
- The guide response is plain text, not a provenance envelope (spec §5.1).
- Docs are part of the change (`CLAUDE.md`): Task 3 is not optional.
- Before each commit: `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass. Before the last commit also `staticcheck ./...` and `deadcode ./cmd/hobbs` (with `GOTOOLCHAIN=go1.25.0`; install as `CLAUDE.md` says); `deadcode` must report only the four items in `.github/deadcode-expected.txt`.
- Commit on the current branch. Do not open a pull request or merge; the maintainer asks for those.
- No end-to-end test here: the hosted server runs `main`, so that check happens after merge and deploy (spec §6).

## Review Focus

- A client that calls `get_briefing_guide` with unexpected arguments (e.g. `{"airports": [...]}`): it should still get the guide or a clear input error, never a crash. (Test in Task 1.)
- An airworthy server: the guide must not start with the NOT AIRWORTHY sentence. (Test in Task 1.)
- A later rename of a tool or field the guide names: the guide would silently mislead agents. (Test in Task 1 pins the names.)
- The Docker image: `guide.md` must be inside the build context so `go:embed` finds it. `.dockerignore` must not exclude `*.md` under `internal/`. (Check in Task 1, step 6.)
- The pointer sentence drifting between the instructions and the description. (Task 2 uses one constant for both.)

---

### Task 1: The guide and its tool

**Files:**
- Create: `internal/mcpserver/guide.md` (the text of spec §5.3, the content of the fenced block, exactly)
- Create: `internal/mcpserver/guide.go`
- Modify: `internal/mcpserver/server.go` (`New`: call `registerGuide(srv, d)` after `registerBriefing`)
- Test: `internal/mcpserver/guide_test.go`; modify `internal/mcpserver/server_test.go` (`TestToolsAreListedWithTheContractInTheirDescriptions`)

**Interfaces:**
- Consumes: `Deps.describe(string) string`, `readOnly(title string) *mcp.ToolAnnotations`, and the test helpers `newUpstreams`, `deps(t, withNMS, airworthy)`, `connect`, `text(res)` in `server_test.go`.
- Produces: `func registerGuide(srv *mcp.Server, d Deps)`; `type GuideInput struct{}`; the package variable `guideText string` (the embedded file); the tool name `get_briefing_guide`.

- [ ] **Step 1: Write the failing tests** in `guide_test.go`

```go
func TestBriefingGuideIsReturnedAsText(t *testing.T) {
	for _, airworthy := range []bool{false, true} {
		cs := connect(t, newUpstreams(t).deps(t, true, airworthy))
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_briefing_guide"})
		if err != nil || res.IsError {
			t.Fatalf("airworthy=%v: err=%v isError=%v", airworthy, err, res != nil && res.IsError)
		}
		got := text(res)
		if !strings.Contains(got, "# Briefing a pilot with hobbs") {
			t.Errorf("airworthy=%v: guide heading missing", airworthy)
		}
		if strings.HasPrefix(got, "NOT AIRWORTHY") == airworthy {
			t.Errorf("airworthy=%v: NOT AIRWORTHY prefix is wrong: %.60q", airworthy, got)
		}
	}
}

// The guide names real tools and fields; a rename must break this test.
func TestBriefingGuideNamesWhatExists(t *testing.T) {
	for _, must := range []string{"get_flight_briefing_data", "`centers`", "`completeness.status`", "`airworthy`", "up to 10", "go/no-go"} {
		if !strings.Contains(guideText, must) {
			t.Errorf("guide lacks %q", must)
		}
	}
}

func TestBriefingGuideIgnoresUnexpectedArguments(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_briefing_guide", Arguments: map[string]any{"airports": []string{"KSMO"}}})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError && !strings.Contains(text(res), "# Briefing a pilot with hobbs") {
		t.Error("neither the guide nor an input error")
	}
}
```

In `server_test.go`, `TestToolsAreListedWithTheContractInTheirDescriptions`: change the count check to `len(got) != 3` with the message `want exactly 3: get_flight_briefing_data, source_status and get_briefing_guide (no TFR tool, no route tool)`, and add after the loop:

```go
	g := got["get_briefing_guide"]
	if g == nil {
		t.Fatal("missing tool get_briefing_guide")
	}
	if g.Annotations == nil || !g.Annotations.ReadOnlyHint {
		t.Error("get_briefing_guide must be annotated read-only")
	}
	if !strings.Contains(g.Description, "before briefing a pilot") || strings.Contains(g.Description, "raw_text") {
		t.Errorf("get_briefing_guide description: want the when-to-call line and no reliance text, got %q", g.Description)
	}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/mcpserver/ -run 'BriefingGuide|ToolsAreListed'`
Expected: FAIL (does not compile: `guideText` undefined).

- [ ] **Step 3: Create `guide.md` and implement `guide.go`**

`guide.md`: the content of the fenced block in spec §5.3, exactly, ending with one newline.

`guide.go`: `//go:embed guide.md` into `var guideText string`; `type GuideInput struct{}`; `func registerGuide(srv *mcp.Server, d Deps)` adds the tool with:
- Name `get_briefing_guide`; annotations `readOnly("How to brief a pilot with this server's data")`, then set `OpenWorldHint` to false on the returned value (the tool reaches nothing outside the server).
- Description, through `d.describe`, without `reliance`: `Returns hobbs's short guide to briefing a general aviation pilot with this server's data: what to ask the pilot, what to fetch, and how to cut the result to a short briefing without losing the data-quality warnings. Call it once before briefing a pilot. It is advice on method, written by the hobbs project, not data from the FAA or NWS, and it fetches nothing.`
- Handler: returns `&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: d.describe(guideText)}}}`, no structured content.

Give the file a comment that says why this tool does not go through `respond` (advice, not source data; spec §5.1).

- [ ] **Step 4: Register it** in `New` in `server.go`, after `registerBriefing(srv, d)`.

- [ ] **Step 5: Run the package tests**

Run: `go test ./internal/mcpserver/`
Expected: PASS. If `TestBriefingGuideIsReturnedAsText` fails on the description casing, fix the test's expected substring to match the description in Step 3, not the other way round.

- [ ] **Step 6: Check the Docker build context**

Run: `cat .dockerignore`
Expected: no pattern that excludes `internal/mcpserver/guide.md` (e.g. `*.md` or `**/*.md`). If one does, add `!internal/mcpserver/guide.md` after it.

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./...
git add internal/mcpserver/guide.md internal/mcpserver/guide.go internal/mcpserver/guide_test.go internal/mcpserver/server.go internal/mcpserver/server_test.go .dockerignore
git commit -m "Add get_briefing_guide: a briefing guide served as a tool"
```

### Task 2: Point the agent at the guide

**Files:**
- Modify: `internal/mcpserver/server.go` (the `Instructions` string in `New`; a new constant)
- Modify: `internal/mcpserver/briefing.go` (`registerBriefing`: the description)
- Test: `internal/mcpserver/guide_test.go`

**Interfaces:**
- Consumes: the tool name `get_briefing_guide` from Task 1.
- Produces: `const guidePointer = "Before briefing a pilot, call get_briefing_guide once and follow it."` in `server.go`.

- [ ] **Step 1: Write the failing test** in `guide_test.go`

```go
func TestInstructionsAndDataToolPointAtTheGuide(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	const want = "Before briefing a pilot, call get_briefing_guide once and follow it."
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, want) {
		t.Errorf("instructions lack the pointer: %q", got)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "get_flight_briefing_data" && !strings.Contains(tl.Description, want) {
			t.Errorf("get_flight_briefing_data description lacks the pointer")
		}
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/mcpserver/ -run PointAtTheGuide`
Expected: FAIL ("instructions lack the pointer").

- [ ] **Step 3: Add `guidePointer` and use it twice**

- `server.go`: in the `Instructions` text, insert `guidePointer + " "` after the sentence ending `source_status checks each source live. `.
- `briefing.go`: in the description, insert `guidePointer + " "` after the first sentence (the one ending `each in its own sublist. `).

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/mcpserver/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./...
git add internal/mcpserver/server.go internal/mcpserver/briefing.go internal/mcpserver/guide_test.go
git commit -m "Point agents at get_briefing_guide from the instructions and the data tool"
```

### Task 3: Docs

**Files:**
- Modify: `docs/DESIGN.md`, `README.md`, `CLAUDE.md`, `internal/mcpserver/server.go` (package comment), `docs/superpowers/specs/2026-11-25-briefing-guide-design.md` (status line)

**Interfaces:**
- Consumes: the names and behavior from Tasks 1 and 2. Read the code as built, not this plan, for the final wording.

Write for a reader with no chat history; date what is dated (2026-11-25); leave no two docs disagreeing. Search first: `grep -n -i "two read-only tools\|one data tool\|every response\|exactly 2\|source_status" README.md CLAUDE.md docs/DESIGN.md` and fix every hit the change makes untrue.

- [ ] **Step 1: `docs/DESIGN.md`**

- §1 (purpose and status): mention the guide tool and the date, in the section's existing style.
- §2: after the paragraph "This layer is the foundation...", state the line as it now stands: the server makes no judgment on records (nothing labelled, ranked, filtered or dropped); since 2026-11-25 it also serves advice on **method** (`get_briefing_guide`), which the agent applies; the judging stays in the agent. Say the risk this accepts: an agent's bad cut can leave a record out of a pilot's briefing, and the guide answers it with "when not sure, keep it in" and the never-leave-out lines.
- §3.1: the pilot as the person the agent briefs, in one line.
- §3.2: a new flow, **F1a. Brief a pilot**: the agent calls `get_briefing_guide`, asks the pilot, calls `get_flight_briefing_data` with the airports and centers it chose along the route, briefs short, keeps the three data-quality lines, closes in one line. Renumber nothing.
- §3.3 R1 and the envelope wording in §4 and §5.1: "every response" becomes "every data response" where the guide is the exception, with a pointer to §5.2.
- §3.4: reword the first non-goal so it reads: relevance scoring, ranking, prioritizing or summarizing **by the server**, and labels on records; advice on method is served by `get_briefing_guide` (§5.2). Keep "Flight-planning decisions or go/no-go advice" as it is.
- §4: add `get_briefing_guide` to the Tools node of the Mermaid diagram (`get_flight_briefing_data · source_status · get_briefing_guide`), and change "Two read-only tools" in the parts list to three, saying the guide tool returns embedded text and touches no source or envelope.
- §5.2: a new `#### \`get_briefing_guide\`` sub-section after `source_status`: no input; plain Markdown text, prefixed with the NOT AIRWORTHY sentence when not airworthy; not an envelope and why; where the text lives (`internal/mcpserver/guide.md`); the two pointers and their exact sentence; what the guide tells the agent (its five steps, one line each).
- §5.7 (code layout): `guide.go`, `guide.md`.
- §6 (testing): the guide's unit tests and the end-to-end check from spec §6.
- §8 (open items): "Does the agent call the guide unprompted? To be observed after deploy; if not, move the method into the data tool's description (alternative A2)."
- §9: one row, dated 2026-11-25, **Scope: the server serves a briefing guide.** Decision and reason from spec §1 and §3 (safety system for general aviation pilots; signal-to-noise; short briefing with a one-line mention of what was left out, the maintainer's choice). Alternatives and why they lost, from spec §4: instructions plus a prompt; the data tool's description; a field in every response; a plugin with skills; MCP prompts and resources alone; and "a full account of what was set aside" (lost to brevity).

- [ ] **Step 2: `README.md`**

Add the tool to the tools table (one row, same style), and one sentence under it on what the guide is for and that it is advice, not data.

- [ ] **Step 3: `CLAUDE.md`**

- Line 3: add the guide tool to the summary.
- "The rules that matter most": the relevance rule now says the server judges no record, and that method advice lives only in `internal/mcpserver/guide.md`; "Every response carries provenance" becomes every data response, the guide being plain text.
- "How to work", end-to-end tests: add `mcp__hobbs__get_briefing_guide` to the tool names, and how to test a guide change: unit tests, then after deploy a fresh session that asks for a briefing, checking the agent called the guide first and the briefing is short with the data-quality lines.

- [ ] **Step 4: Package comment and spec status**

- `server.go` package comment: one sentence on the guide tool as the one response that is not an envelope.
- The spec's status line: `implemented, 2026-11-25`, naming the branch, and "`docs/DESIGN.md` is now the source of truth".

- [ ] **Step 5: Verify and commit**

```bash
gofmt -l . && go vet ./... && go test ./...
GOTOOLCHAIN=go1.25.0 staticcheck ./... && GOTOOLCHAIN=go1.25.0 deadcode ./cmd/hobbs
grep -n -i "two read-only tools\|exactly 2" README.md CLAUDE.md docs/DESIGN.md   # expect no output
git add -A docs README.md CLAUDE.md internal/mcpserver/server.go
git commit -m "Document get_briefing_guide and the method-advice line in the principles"
```

Expected: tests pass; `staticcheck` prints nothing; `deadcode` prints only the four expected items.
