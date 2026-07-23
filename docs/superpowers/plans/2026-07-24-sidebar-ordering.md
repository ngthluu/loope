# Sidebar Ordering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorder the dashboard sidebar so active tickets sort to the top by attention-priority tier and done tickets sink to the bottom, with issue Number descending (newest first) within each tier.

**Architecture:** Replace the `LastActive`-based comparison in `sortTickets` (`tracker.go`) with a two-key sort: a status-tier rank (derived from the existing `stateKind` classifier in `render.go`) as the primary key, and issue `Number` descending as the secondary key. Both `sortTickets` and a new `statusRank` helper take a `*Config` because `stateKind` needs the project's configurable `StateLabels`. `scanLogs` gains a `*Config` parameter so it can pass `cfg` to `sortTickets`; its two callers already hold a `*Config`.

**Tech Stack:** Go (standard library `sort`, `testing`).

## Global Constraints

- The sort must stay `sort.SliceStable` and produce a deterministic total order (tier + Number, no duplicate keys since issue Numbers are unique) so the rail does not jitter between refreshes.
- `LastActive` must no longer be used for ordering. Do **not** remove the `LastActive` field from the `Ticket` struct or stop populating it — only its use in `sortTickets` goes away.
- No template, CSS, or route changes. The rail renders whatever order the tracker returns.
- No new configuration knobs; the tier order is fixed.
- Reuse the existing `stateKind(cfg, label)` classifier from `render.go` — do not re-implement label matching.

### Status tier ranks (fixed, from the spec)

| Rank | `stateKind` | Meaning                          |
|------|-------------|----------------------------------|
| 0    | `failed`    | Needs human attention (blocked)  |
| 1    | `rework`    | Needs human attention (revise)   |
| 2    | `wip`       | Actively in progress             |
| 3    | `queued`    | Waiting to be picked up          |
| 4    | `""`        | Unknown / other                  |
| 5    | `done`      | Complete — pinned to the bottom  |

---

## Assumptions (headless-mode decisions)

- The new `sortTickets(cfg *Config, tickets []Ticket)` and `statusRank(cfg *Config, label string)` live in `tracker.go`, matching the spec's implementation sketch and keeping sort logic beside its only callers.
- Tests are added to the existing `tracker_test.go` and drive `sortTickets` directly with in-memory `[]Ticket` values (no disk fixtures needed), since the sort is a pure function of `StateLabel` and `Number`. This is the smallest, most direct way to assert ordering.
- Test configs use `defaultStateLabels()` (already used throughout `tracker_test.go`) except the custom-label test, which builds an explicit `StateLabels`.
- `scanLogs`'s signature becomes `scanLogs(cfg *Config)` and it derives `logsDir` from `cfg.WorkDir` (the spec's suggested shape). Its doc-comment is updated to drop the stale "sorted by LastActive descending" claim.

## File Structure

- `tracker.go` — Modify. Rewrite `sortTickets` and its doc-comment; add `statusRank`; change `scanLogs` to take `*Config`; update the `sortTickets` call in `overlayIssues` and the `BuildTickets` call to `scanLogs`.
- `serve.go` — Modify. Update the `scanLogs(s.cfg.WorkDir)` call to `scanLogs(s.cfg)`.
- `tracker_test.go` — Modify. Add the sort tests; update any existing `scanLogs(...)` call sites to the new signature.

---

### Task 1: Add `statusRank` and rewrite `sortTickets` (config-aware two-key sort)

**Files:**
- Modify: `tracker.go` (add `statusRank`; rewrite `sortTickets` at `tracker.go:308-318`)
- Test: `tracker_test.go`

**Interfaces:**
- Consumes: `stateKind(cfg *Config, label string) string` from `render.go` (returns one of `"failed"`, `"rework"`, `"wip"`, `"queued"`, `"done"`, `""`); `Ticket.StateLabel` and `Ticket.Number` fields from `tracker.go`; `*Config` with `StateLabels StateLabels` and `EligibleLabel string`; `defaultStateLabels()` from `config.go`.
- Produces:
  - `statusRank(cfg *Config, label string) int` — maps a state label to its tier rank (0 failed, 1 rework, 2 wip, 3 queued, 4 unknown/`""`, 5 done).
  - `sortTickets(cfg *Config, tickets []Ticket)` — **new signature** (was `sortTickets(tickets []Ticket)`). Stable-sorts in place by tier rank ascending, then `Number` descending.

- [ ] **Step 1: Write the failing tests**

Add these tests to `tracker_test.go`. They call `sortTickets` with the new `(cfg, tickets)` signature — this is what makes them fail to compile until the implementation changes.

```go
// mkTicket builds a minimal ticket carrying only the fields the sidebar sort
// reads: its issue Number and state label.
func mkTicket(number int, label string) Ticket {
	return Ticket{Number: number, StateLabel: label}
}

// numbers returns the ticket Numbers in slice order, for compact assertions.
func numbers(tickets []Ticket) []int {
	out := make([]int, len(tickets))
	for i, t := range tickets {
		out[i] = t.Number
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSortTicketsDoneSinksToBottom(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	sl := defaultStateLabels()
	tickets := []Ticket{
		mkTicket(21, sl.Done),
		mkTicket(2, sl.WIP),
		mkTicket(13, sl.Done),
		mkTicket(6, sl.Rework),
	}
	sortTickets(cfg, tickets)
	// Every active ticket precedes every done ticket, regardless of Number.
	got := numbers(tickets)
	want := []int{6, 2, 21, 13} // rework, wip, then done by Number desc
	if !equalInts(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSortTicketsAttentionTiers(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	sl := defaultStateLabels()
	// One ticket per bucket, deliberately shuffled on input.
	tickets := []Ticket{
		mkTicket(1, sl.Done),
		mkTicket(2, "some-unknown-label"),
		mkTicket(3, sl.WIP),
		mkTicket(4, cfg.EligibleLabel), // queued
		mkTicket(5, sl.Rework),
		mkTicket(6, sl.Failed),
	}
	sortTickets(cfg, tickets)
	got := numbers(tickets)
	// failed -> rework -> wip -> queued -> unknown -> done
	want := []int{6, 5, 3, 4, 2, 1}
	if !equalInts(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSortTicketsNumberDescendingWithinTier(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	sl := defaultStateLabels()
	tickets := []Ticket{
		mkTicket(4, sl.WIP),
		mkTicket(21, sl.WIP),
		mkTicket(1, sl.WIP),
		mkTicket(13, sl.WIP),
	}
	sortTickets(cfg, tickets)
	got := numbers(tickets)
	want := []int{21, 13, 4, 1}
	if !equalInts(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSortTicketsCustomLabelsHonored(t *testing.T) {
	cfg := &Config{
		EligibleLabel: "queued-please",
		StateLabels: StateLabels{
			WIP:    "in-progress",
			Failed: "blocked",
			Done:   "shipped",
			Rework: "revise",
		},
	}
	tickets := []Ticket{
		mkTicket(1, "shipped"),      // done -> rank 5
		mkTicket(2, "in-progress"),  // wip  -> rank 2
		mkTicket(3, "blocked"),      // failed -> rank 0
		mkTicket(4, "queued-please"), // queued -> rank 3
		mkTicket(5, "revise"),       // rework -> rank 1
	}
	sortTickets(cfg, tickets)
	got := numbers(tickets)
	want := []int{3, 5, 2, 4, 1} // blocked, revise, in-progress, queued, shipped
	if !equalInts(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSortTicketsDeterministicNoOp(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	sl := defaultStateLabels()
	tickets := []Ticket{
		mkTicket(6, sl.Failed),
		mkTicket(5, sl.Rework),
		mkTicket(3, sl.WIP),
		mkTicket(4, cfg.EligibleLabel),
		mkTicket(2, "unknown"),
		mkTicket(1, sl.Done),
	}
	sortTickets(cfg, tickets)
	first := numbers(tickets)
	sortTickets(cfg, tickets) // sorting an already-sorted slice must not change it
	second := numbers(tickets)
	if !equalInts(first, second) {
		t.Fatalf("second sort changed order: %v -> %v", first, second)
	}
}

func TestStatusRank(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	sl := defaultStateLabels()
	cases := []struct {
		label string
		want  int
	}{
		{sl.Failed, 0},
		{sl.Rework, 1},
		{sl.WIP, 2},
		{cfg.EligibleLabel, 3},
		{"totally-unknown", 4},
		{"", 4},
		{sl.Done, 5},
	}
	for _, c := range cases {
		if got := statusRank(cfg, c.label); got != c.want {
			t.Errorf("statusRank(%q) = %d, want %d", c.label, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestSortTickets|TestStatusRank'`
Expected: FAIL — compile error `too many arguments in call to sortTickets` and `undefined: statusRank`.

- [ ] **Step 3: Add `statusRank` and rewrite `sortTickets` in `tracker.go`**

Replace the existing `sortTickets` block (`tracker.go:308-318`):

```go
// sortTickets orders tickets by LastActive descending, breaking ties by
// ascending Number so tickets sharing a zero LastActive (label-only, no
// logs yet) don't jitter between refreshes.
func sortTickets(tickets []Ticket) {
	sort.SliceStable(tickets, func(i, j int) bool {
		if !tickets[i].LastActive.Equal(tickets[j].LastActive) {
			return tickets[i].LastActive.After(tickets[j].LastActive)
		}
		return tickets[i].Number < tickets[j].Number
	})
}
```

with:

```go
// statusRank orders tickets in the sidebar: lower rank sorts higher. Active
// states that need attention float to the top; done sinks to the bottom. It
// reuses stateKind (render.go) so renamed StateLabels still tier correctly.
func statusRank(cfg *Config, label string) int {
	switch stateKind(cfg, label) {
	case "failed":
		return 0
	case "rework":
		return 1
	case "wip":
		return 2
	case "queued":
		return 3
	case "done":
		return 5
	default: // "" unknown/other
		return 4
	}
}

// sortTickets orders tickets by attention-priority status tier, then by issue
// Number descending within each tier. The combination of (tier, Number) is a
// total order with no duplicate keys (issue Numbers are unique), so the result
// is deterministic and does not jitter between refreshes.
func sortTickets(cfg *Config, tickets []Ticket) {
	sort.SliceStable(tickets, func(i, j int) bool {
		ri, rj := statusRank(cfg, tickets[i].StateLabel), statusRank(cfg, tickets[j].StateLabel)
		if ri != rj {
			return ri < rj
		}
		return tickets[i].Number > tickets[j].Number
	})
}
```

- [ ] **Step 4: Update the two `sortTickets` call sites in `tracker.go`**

At the end of `scanLogs` (`tracker.go:301`), the call is inside `scanLogs`, which does not yet hold a `cfg` — Task 2 threads that in. For now, to keep the file compiling in isolation is **not** required (Task 2 immediately follows and lands the `cfg` plumbing). Update the `overlayIssues` call site (`tracker.go:610`), which already has `cfg`:

Change:

```go
	sortTickets(tickets)
	return tickets
```

to:

```go
	sortTickets(cfg, tickets)
	return tickets
```

Leave the `scanLogs` call site for Task 2.

- [ ] **Step 5: Commit**

This commit will not yet build on its own because `scanLogs` still calls the old `sortTickets(tickets)`; Task 2 completes the plumbing. If you prefer a green intermediate commit, do Task 2's Steps 1–3 before committing. Recommended: fold Task 2 in, then commit once. To commit now regardless:

```bash
git add tracker.go tracker_test.go
git commit -m "feat: attention-priority sidebar sort (statusRank + two-key sortTickets)"
```

---

### Task 2: Thread `*Config` into `scanLogs` and update all call sites

**Files:**
- Modify: `tracker.go` (`scanLogs` signature + doc-comment at `tracker.go:275-306`; the `scanLogs` call in `BuildTickets` at `tracker.go:618`)
- Modify: `serve.go` (`scanLogs` call at `serve.go:115`)
- Test: `tracker_test.go` (update existing `scanLogs(...)` call sites)

**Interfaces:**
- Consumes: `sortTickets(cfg *Config, tickets []Ticket)` from Task 1; `*Config` with `WorkDir string`.
- Produces: `scanLogs(cfg *Config) ([]Ticket, error)` — **new signature** (was `scanLogs(workDir string)`). Reads `cfg.WorkDir/logs`.

- [ ] **Step 1: Find every `scanLogs` call site**

Run: `grep -rn "scanLogs(" *.go`
Expected: hits in `tracker.go` (definition + `BuildTickets`), `serve.go` (`load`), and possibly `tracker_test.go`. Note each one — all must be updated.

- [ ] **Step 2: Change the `scanLogs` signature and doc-comment in `tracker.go`**

Replace the doc-comment and signature (`tracker.go:275-279`):

```go
// scanLogs reads workDir/logs and returns one Ticket per issue-<N> dir, steps
// ordered by seq and cost summed, sorted by LastActive descending. A missing
// logs dir yields an empty slice, not an error.
func scanLogs(workDir string) ([]Ticket, error) {
	logsDir := filepath.Join(workDir, "logs")
```

with:

```go
// scanLogs reads cfg.WorkDir/logs and returns one Ticket per issue-<N> dir,
// steps ordered by seq and cost summed, sorted by attention-priority status
// tier then Number descending (see sortTickets). A missing logs dir yields an
// empty slice, not an error.
func scanLogs(cfg *Config) ([]Ticket, error) {
	logsDir := filepath.Join(cfg.WorkDir, "logs")
```

- [ ] **Step 3: Update the `sortTickets` call inside `scanLogs`**

In `scanLogs` (`tracker.go:301`), change:

```go
	sortTickets(tickets)
```

to:

```go
	sortTickets(cfg, tickets)
```

- [ ] **Step 4: Update the `scanLogs` call in `BuildTickets`**

`BuildTickets` (`tracker.go:617-618`) already has `cfg`. Change:

```go
	tickets, err := scanLogs(cfg.WorkDir)
```

to:

```go
	tickets, err := scanLogs(cfg)
```

- [ ] **Step 5: Update the `scanLogs` call in `serve.go`**

In `Server.load` (`serve.go:115`), change:

```go
	tickets, err := scanLogs(s.cfg.WorkDir)
```

to:

```go
	tickets, err := scanLogs(s.cfg)
```

- [ ] **Step 6: Update `scanLogs` call sites in `tracker_test.go`**

For each hit from Step 1 in `tracker_test.go`, replace `scanLogs(work)` (or `scanLogs(someWorkDir)`) with a `cfg`-based call. A test that already has a `cfg` in scope uses it directly:

```go
	tickets, err := scanLogs(cfg)
```

A test with only a `work` dir string and no `cfg` builds a minimal one inline:

```go
	tickets, err := scanLogs(&Config{WorkDir: work, StateLabels: defaultStateLabels()})
```

Preserve each test's existing error handling and assertions verbatim; only the call expression changes.

- [ ] **Step 7: Build to verify everything compiles**

Run: `go build ./...`
Expected: no output (success). If `go vet` is part of the project flow: `go vet ./...` — no output.

- [ ] **Step 8: Run the full test suite**

Run: `go test ./...`
Expected: PASS (all packages `ok`), including the Task 1 sort tests and every pre-existing tracker/serve test.

- [ ] **Step 9: Commit**

```bash
git add tracker.go serve.go tracker_test.go
git commit -m "feat: thread cfg into scanLogs so sidebar sort sees StateLabels"
```

---

## Self-Review

**Spec coverage:**

- Sort by Number descending within tier → Task 1, `TestSortTicketsNumberDescendingWithinTier`.
- Done sinks to bottom → Task 1, `TestSortTicketsDoneSinksToBottom`.
- Active tickets on top by attention tier → Task 1, `TestSortTicketsAttentionTiers`.
- `statusRank` tier mapping (0 failed … 5 done, `""` = 4) → Task 1, `TestStatusRank` + implementation.
- Two-key `sort.SliceStable` (tier, Number desc), `LastActive` no longer used → Task 1 Step 3.
- `statusRank`/`sortTickets` take `*Config`; both call sites pass `cfg` → Task 1 Step 4 (`overlayIssues`) + Task 2 Steps 3–4 (`scanLogs`, `BuildTickets`).
- `scanLogs` gains `*Config`; `serve.go` handler updated → Task 2 Steps 2, 5.
- Custom labels honored → Task 1, `TestSortTicketsCustomLabelsHonored`.
- Determinism / stable across refreshes → Task 1, `TestSortTicketsDeterministicNoOp`.
- No template/CSS/route changes; no new config knobs → nothing in those files is touched (Global Constraints).

**Placeholder scan:** No TBD/TODO/"add error handling"/"similar to Task N" — all code and commands are literal.

**Type consistency:** `statusRank(cfg *Config, label string) int`, `sortTickets(cfg *Config, tickets []Ticket)`, and `scanLogs(cfg *Config) ([]Ticket, error)` are used identically across every task and call site. `stateKind`'s return strings (`"failed"`, `"rework"`, `"wip"`, `"queued"`, `"done"`, `""`) match the `statusRank` switch cases. `StateLabels` fields (`WIP`, `Failed`, `Done`, `Rework`) and `EligibleLabel` match `config.go`.

**Note on commit boundaries:** Task 1 alone leaves `scanLogs` calling the old `sortTickets(tickets)` (a compile error). The two tasks are split for reviewability of the sort logic vs. the plumbing; if you want each commit to build green, run Task 2 before committing Task 1 (Task 1 Step 5 notes this). The suggested path: implement Task 1 code, then Task 2 code, verify `go build ./...` and `go test ./...`, then make the two commits (or a single squashed commit).
