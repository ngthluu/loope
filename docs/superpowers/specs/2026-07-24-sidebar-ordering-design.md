# Sidebar ordering — design (#24)

## Problem

The dashboard sidebar (the queue rail) lists tickets in an order that reads as
arbitrary: a recently-touched *done* ticket can sit above an in-progress one,
and tickets are newest-*last* on ties. The screenshot for #24 shows a WIP ticket
on top followed by an unordered run of done tickets (#21, #2, #1, #4, #6, #13).

The cause is the current sort in `tracker.go`:

```go
// sortTickets orders tickets by LastActive descending, breaking ties by
// ascending Number ...
func sortTickets(tickets []Ticket) {
	sort.SliceStable(tickets, func(i, j int) bool {
		if !tickets[i].LastActive.Equal(tickets[j].LastActive) {
			return tickets[i].LastActive.After(tickets[j].LastActive)
		}
		return tickets[i].Number < tickets[j].Number
	})
}
```

Ordering by `LastActive` mixes done and active tickets together, and the
`Number` ascending tie-break puts older issues first.

## Goal

Order the sidebar so that:

1. Tickets are sorted by **issue Number descending** (newest first).
2. **Done** tickets always sink to the **bottom**.
3. **Active** tickets (WIP, rework, failed, queued) stay on **top**, ordered by
   an attention-priority tier so the states a human must act on rise highest.

## Design

Replace the `LastActive`-based comparison in `sortTickets` with a **two-key
sort**:

1. **Primary key — status tier** (ascending rank; lower rank = higher in the
   list).
2. **Secondary key — `Number` descending** within each tier.

`LastActive` is no longer used for ordering.

### Status tiers (attention-priority — Option B)

A helper maps each ticket to a rank via the existing `stateKind` classifier
(`render.go`), which already collapses configurable GitHub labels into the
semantic buckets `wip`, `rework`, `failed`, `queued`, `done`, and `""`
(unknown/other):

| Rank | `stateKind` | Meaning                          |
|------|-------------|----------------------------------|
| 0    | `failed`    | Needs human attention (blocked)  |
| 1    | `rework`    | Needs human attention (revise)   |
| 2    | `wip`       | Actively in progress             |
| 3    | `queued`    | Waiting to be picked up          |
| 4    | `""`        | Unknown / other                  |
| 5    | `done`      | Complete — pinned to the bottom  |

Rationale: the states a human must act on (`failed`, `rework`) rise highest;
in-progress work sits below them; idle `queued` tickets sink toward the bottom
just above unknown and done; `done` is always last.

### Implementation sketch

```go
// statusRank orders tickets in the sidebar: lower rank sorts higher. Active
// states that need attention float to the top; done sinks to the bottom.
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
// Number descending within each tier. The total order (tier + Number) is
// stable across refreshes.
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

`statusRank` and `sortTickets` take `*Config` because `stateKind` needs the
project's configurable `StateLabels` to classify a ticket. `sortTickets` has two
call sites:

- `overlayIssues` (`tracker.go`) — already holds a `*Config`; pass it through.
- `scanLogs` (`tracker.go`) — does **not** currently take a `*Config`. Thread
  one in (e.g. `scanLogs(cfg *Config)`, deriving `workDir` from `cfg.WorkDir`).
  Its two callers, `BuildTickets` (`tracker.go`) and the serve handler
  (`serve.go`), both already hold a `*Config`, so no further plumbing is needed.

## Affected code

- `tracker.go` — rewrite `sortTickets` (and its doc-comment); add `statusRank`
  (reusing `stateKind` from `render.go`); pass `cfg` at both `sortTickets` call
  sites; add a `*Config` parameter to `scanLogs`.
- `serve.go` — update the `scanLogs` call to pass the handler's `cfg`.

No template, CSS, or route changes: the rail renders whatever order the tracker
returns.

## Ordering stability

The sort remains `sort.SliceStable`, and the combination of (tier, Number) is a
**total order** with no duplicate keys (issue Numbers are unique), so the result
is deterministic and does not jitter between refreshes. This preserves the
guarantee the original tie-break comment was protecting, now without depending
on `LastActive`.

## Testing

Extend the existing tracker tests (`tracker_test.go`):

- **Done sinks to bottom:** a mix of done and active tickets — every done
  ticket sorts after every active ticket, regardless of Number.
- **Attention-priority tiers:** one ticket per bucket
  (`failed`, `rework`, `wip`, `queued`, unknown, `done`) → asserts the exact
  order failed → rework → wip → queued → unknown → done.
- **Number descending within a tier:** several tickets in the same bucket sort
  by Number descending.
- **Custom labels honored:** a `Config` with renamed `StateLabels` still tiers
  correctly (guards the `stateKind`/`cfg` path).
- **Determinism:** sorting an already-sorted slice is a no-op (stable across
  refreshes).

## Out of scope

- No change to which tickets are shown, their live/refresh behavior, or the
  rail's visual styling.
- No new configuration knobs; the tier order is fixed.
