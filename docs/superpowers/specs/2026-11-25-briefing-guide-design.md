# A briefing guide served by the server: `get_briefing_guide` (design)

Date: 2026-11-25. Status: **proposed, awaiting the maintainer's approval.** Once built, `docs/DESIGN.md` is the source of truth and this file is kept as the design record.

## 1. Intent

An agent that connects to hobbs today gets the data but not a way to brief with it. It may ask for too little (only the two airports), or hand the pilot hundreds of NOTAMs unsorted. The maintainer sees hobbs as part of **a safety system for general aviation pilots**, and in that frame the noise is itself the hazard: a pilot given 200 NOTAMs stops reading. The value is the signal-to-noise ratio of the briefing.

So the server will also serve a short **briefing guide**: instructions for the agent on how to ask the pilot, what to fetch, and how to cut the result down to a short briefing. Every agent that connects gets it, with nothing to install.

## 2. Goals, non-goals, anti-goals

**Goals**

- A guide that makes the agent: ask the pilot the few questions that let it filter; fetch widely (including airports and centers along the route, chosen by the agent); brief only what could hurt or stop this flight; never cut the data-quality lines; say in one line what it left out.
- Delivered so the agent gets it without the pilot doing anything, in any MCP client.
- No cost in conversations that have nothing to do with a briefing.

**Non-goals**

- Any change to `get_flight_briefing_data` or to the data it returns. The server still returns everything, unranked and unfiltered.
- A route input, or the server inferring airports or centers along a route (`docs/DESIGN.md` §3.4, §9). The agent picks them and passes them in `airports` and `centers`, which the tool already takes.
- Go/no-go advice. The guide tells the agent not to give it.
- A plugin or client-side skills, MCP prompts, and MCP resources (§4).

**Anti-goals (a failure even if it works)**

- A long briefing. A complete list of what was set aside, or caveats on every line, defeats the purpose.
- A briefing that drops a data-pull problem, the missing TFR outlines or the NOT AIRWORTHY mark. Those are signal, not noise.
- Guidance that changes which records the server returns.

## 3. What changes in the principles

`docs/DESIGN.md` §3.4 lists `"you should care about this" guidance` as a non-goal, and §2 keeps relevance out of this layer. This design keeps the data path as it is and adds **advice on method**: the server tells the agent how to brief, and the agent does the judging, as §3.2 F1 step 4 already says it does. The server still never labels, ranks or drops a record.

It is still a change of scope (the server now ships briefing advice), and it moves a risk: a bad cut by the agent can leave a record out of the pilot's briefing. The maintainer accepted this on 2026-11-25, choosing a short briefing with a one-line mention of what was left out over a full account of it. It gets a §9 entry, and §2 and §3.4 are reworded to say where the line now is: **no judgment on records in the server; advice on method is allowed.**

## 4. Approaches considered

- **A. In the server `instructions`, plus an MCP prompt.** Always reaches the agent. Rejected: the instructions sit in the context of every session where hobbs is connected, whether or not the talk is about flying, and Claude Code cuts long instructions off.
- **A2. In the description of `get_flight_briefing_data`.** Free in clients that hold tool descriptions back until needed, and read just before the first call. Not chosen: that description is already about 400 words, and clients that load every description pay for the guide in every session.
- **B. A guidance field in every data response.** Cannot be missed. Rejected: mixes advice into the data envelope and repeats on every call.
- **C. A separate guide tool that the agent is told to call first (chosen by the maintainer, 2026-11-25).** The guide is loaded only when a briefing is starting. Tools work in every MCP client, which prompts and resources do not. Its weakness: the agent can skip the call. Two pointers reduce that (§5.2).
- **A plugin with skills** (discussed first): skills turn on by themselves, but only in Claude clients, and users must install and update it. Set aside for a server-only answer.

## 5. Design

### 5.1 The tool

`get_briefing_guide`: no input. Read-only. Returns the guide as Markdown text.

The response goes through `respond`, like every tool: an envelope with `data.guide` holding the text, no sources (nothing is fetched), and the NOT AIRWORTHY mark when the server is not airworthy. (The builder's call: one rule for every response and no exception to explain; the mark comes with it.)

Its description is short: what it is, and "call it once before briefing a pilot". It does not carry the `reliance` text, which is about reading data.

### 5.2 The pointers

- Server `instructions`: one added sentence, "Before briefing a pilot, call get_briefing_guide once and follow it."
- `get_flight_briefing_data` description: the same sentence, added at the front after the first sentence.

### 5.3 The guide text

Kept in its own file, `internal/mcpserver/guide.md`, embedded in the binary (`go:embed`), so it can be read and edited as a document. Draft (the maintainer approves the wording with this design; about 300 words):

```markdown
# Briefing a pilot with hobbs

The pilot needs a short briefing. A long one does not get read, so what you leave out matters as much as what you say.

## 1. Ask first
Ask only for what you do not already know: departure time and flight time, aircraft, VFR or IFR, cruise altitude, route, alternates. One short message.

## 2. Fetch wide
Call get_flight_briefing_data once with every airport that matters: departure, destination, alternates, and airports along the route (up to 10, ICAO identifiers). Put the en-route centers the route crosses in `centers`; the server does not work these out. Read `completeness.status` and the issues before anything else.

## 3. Brief short
Report only what could hurt or stop this flight, most serious first:
- closed or restricted runways, airports and airspace, including flight restrictions (they appear as FDC NOTAMs);
- navigation aids, approaches and services this flight would use that are out of service;
- weather at the flight's time, route and altitude: SIGMETs, AIRMETs, CWAs, and METARs and TAFs against what the pilot and aircraft can do;
- anything else you judge a hazard for this pilot, aircraft and flight.
Use the times, altitudes and places in each record to decide. Leave out what cannot affect this flight. When you are not sure, keep it in. Quote identifiers and times as published.

## 4. Never leave out
At the top, in one line each, only when it applies:
- the data is not airworthy (`airworthy` is false): say it is not for flight;
- any data pull that failed or was partial, and what is missing because of it;
- flight restriction outlines are not available here: the pilot must check the FAA's TFR site.

## 5. Close
One line: how many records you reviewed and set aside, and that the pilot can ask for them. Do not list them. Do not give go/no-go advice: the decision is the pilot's.
```

### 5.4 What a briefing then looks like

```
KPAO → KTRK, 14:00Z, C172, VFR 9,500
⚠ Test data, not for flight. NOTAM pull complete; TFR outlines unavailable, check tfr.faa.gov.
1. KTRK RWY 11/29 closed 13:00–18:00Z
2. AIRMET: moderate turbulence below 12,000 over the Sierra
3. KTRK TAF: gusts to 28 kt after 15:00Z
187 other NOTAMs reviewed, none affecting this flight. Ask to see them.
```

## 6. Testing

- Unit (`go test ./...`): the tool is listed (three tools now); its response is an envelope with the guide text and, when not airworthy, the mark; the guide names the real tool and input names (`get_flight_briefing_data`, `centers`, `completeness.status`, `airworthy`), so a rename breaks the test; the instructions and the data tool's description both name `get_briefing_guide`.
- End to end, after merge and deploy, against the hosted server: call `get_briefing_guide`, then run a briefing in a fresh session and check the agent called the guide first and that the briefing has the shape in §5.4.

## 7. Docs to update with the change

- `docs/DESIGN.md`: §2 and §3.4 (the line in §3 above), §3.2 (a flow for briefing a pilot), §4 (the guide tool in the diagram), §5.2 (the tool contract), §9 (the decision, with the alternatives in §4 above).
- `README.md`: the tools list.
- `CLAUDE.md`: the summary line (two data-layer tools plus the guide), the "rules that matter most" wording on relevance, and how to test a change to the guide.

## 8. Left to the builder

- The exact tool and field descriptions, within §5.1 and §5.2.
- Tightening the guide's wording without changing what it tells the agent to do.
