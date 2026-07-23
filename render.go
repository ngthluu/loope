package main

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
)

// This file holds the dashboard's presentation layer: the pure formatters and
// class-pickers the templates call, plus the FuncMap that binds them. serve.go
// is left with HTTP concerns only.

// templateFuncs binds the presentation helpers for one Server. The closures
// capture cfg because label semantics and the issue URL are per-repository.
func templateFuncs(cfg *Config) template.FuncMap {
	return template.FuncMap{
		"money":        money,
		"dollars":      dollars,
		"short":        short,
		"shortid":      shortid,
		"errCount":     errCount,
		"statusChip":   statusChip,
		"nodeClass":    nodeClass,
		"cardClass":    cardClass,
		"divClass":     divClass,
		"stateKind":    func(label string) string { return stateKind(cfg, label) },
		"issueURL":     func(n int) string { return "https://github.com/" + cfg.RepoSlug + "/issues/" + strconv.Itoa(n) },
		"stripeClass":  func(label string) string { return stripeClass(cfg, label) },
		"tokens":       tokens,
		"duration":     duration,
		"ctxTokens":    ctxTokens,
		"hasUsage":     hasUsage,
		"hasAnswerer":  hasAnswerer,
		"pipelineRows": pipelineRows,
		"txLine":       txLine,
	}
}

// hasRunning reports whether any of the ticket's steps is still in flight.
func hasRunning(t Ticket) bool {
	for _, s := range t.Steps {
		if s.Status == StatusRunning {
			return true
		}
	}
	return false
}

// errCount is how many of the ticket's steps ended in error.
func errCount(t Ticket) int {
	n := 0
	for _, s := range t.Steps {
		if s.Status == StatusError {
			n++
		}
	}
	return n
}

// money formats a per-step cost, using an em dash for zero/unknown (a running
// step has no settled cost yet).
func money(c float64) string {
	if c == 0 {
		return "—"
	}
	return "$" + strconv.FormatFloat(c, 'f', 2, 64)
}

// dollars formats an aggregate figure that should always read as money,
// including a genuine zero.
func dollars(c float64) string {
	return fmt.Sprintf("$%.2f", c)
}

// tokens humanizes a token count: exact under 1k, rounded "11k" under 1M, and
// two-decimal "1.46M" beyond.
func tokens(n int) string {
	switch {
	case n < 1000:
		if n < 0 {
			return "0"
		}
		return strconv.Itoa(n)
	case n < 1_000_000:
		return strconv.Itoa(int(math.Round(float64(n)/1000))) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 2, 64) + "M"
	}
}

// duration renders a millisecond span compactly: "45s", "3m26s", "1h02m". Zero
// or negative reads as an em dash.
func duration(ms int) string {
	if ms <= 0 {
		return "—"
	}
	s := ms / 1000
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m := s / 60
	s %= 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m %= 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

// ctxTokens is a step's prompt-side (context) token total: fresh input plus both
// cache tiers.
func ctxTokens(s Step) int {
	return s.InputTokens + s.CacheCreationTokens + s.CacheReadTokens
}

// hasUsage reports whether a step carries any token accounting worth showing.
func hasUsage(s Step) bool {
	return s.NumTurns > 0 || s.OutputTokens > 0 || ctxTokens(s) > 0
}

// txLine renders one transcript event as an inline HTML row: a glyph plus its
// text/tool detail. Text is escaped; only the fixed markup is trusted.
func txLine(e TranscriptEvent) template.HTML {
	esc := template.HTMLEscapeString
	switch e.Kind {
	case "text":
		return template.HTML(`<div class="text-text/85">` + esc(e.Text) + `</div>`)
	case "thinking":
		return template.HTML(`<div class="italic text-faint">` + esc(e.Text) + `</div>`)
	case "tool":
		d := ""
		if e.Detail != "" {
			d = ` <span class="text-faint">` + esc(e.Detail) + `</span>`
		}
		return template.HTML(`<div class="text-live">⚙ <span class="text-text/85">` + esc(e.Tool) + `</span>` + d + `</div>`)
	default: // tool_result
		if e.IsError {
			return template.HTML(`<div class="text-err/80">↳ error</div>`)
		}
		return template.HTML(`<div class="text-ok/80">↳ ok</div>`)
	}
}

// short returns the first n runes of s (for a compact session id column).
func short(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// shortid abbreviates a full session id as head…tail so it fits one cell
// without losing the recognizable ends.
func shortid(s string) string {
	r := []rune(s)
	if len(r) <= 16 {
		return s
	}
	return string(r[:8]) + "…" + string(r[len(r)-6:])
}

// stateKind maps a GitHub state label to a semantic bucket the templates key
// their color and copy on, so the palette stays correct even when a project
// renames its labels.
func stateKind(cfg *Config, label string) string {
	switch label {
	case cfg.StateLabels.Done:
		return "done"
	case cfg.StateLabels.WIP:
		return "wip"
	case cfg.StateLabels.Rework:
		return "rework"
	case cfg.StateLabels.Failed:
		return "failed"
	case cfg.StateLabels.Stopped:
		return "stopped"
	case cfg.EligibleLabel:
		return "queued"
	default:
		return ""
	}
}

// stripeClass is the rail row's left accent-bar color for a state.
func stripeClass(cfg *Config, label string) string {
	switch stateKind(cfg, label) {
	case "done":
		return "bg-ok/50"
	case "wip":
		return "bg-live"
	case "rework":
		return "bg-warn/80"
	case "failed":
		return "bg-err/70"
	case "stopped":
		return "bg-muted/60"
	default:
		return "bg-line2"
	}
}

// nodeClass is the pipeline-spine node's fill + glow for a step status.
func nodeClass(st StepStatus) string {
	switch st {
	case StatusOK:
		return "bg-ok node-ok"
	case StatusError:
		return "bg-err node-err"
	case StatusRunning:
		return "bg-live node-live"
	default:
		return "bg-faint"
	}
}

// cardClass is the step card's surface + ring for a status: quiet for settled
// steps, tinted and outlined for the error and in-flight states.
func cardClass(st StepStatus) string {
	switch st {
	case StatusError:
		return "bg-err/[0.055] ring-1 ring-err/35"
	case StatusRunning:
		return "bg-live/[0.045] ring-1 ring-live/40"
	default:
		return "bg-panel ring-1 ring-line"
	}
}

// divClass is the divider color between a step's header and its disclosures.
func divClass(st StepStatus) string {
	switch st {
	case StatusError:
		return "border-err/25"
	case StatusRunning:
		return "border-live/25"
	default:
		return "border-line/60"
	}
}

// statusChip renders the inline status marker (icon + word) for a step.
func statusChip(st StepStatus) template.HTML {
	switch st {
	case StatusOK:
		return `<span class="inline-flex items-center gap-1 font-mono text-[10px] font-semibold uppercase tracking-wide text-ok"><svg class="h-3 w-3" viewBox="0 0 12 12" fill="none"><path d="M2.5 6.2l2.2 2.2 4.8-5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>ok</span>`
	case StatusError:
		return `<span class="inline-flex items-center gap-1 font-mono text-[10px] font-semibold uppercase tracking-wide text-err"><svg class="h-3 w-3" viewBox="0 0 12 12" fill="none"><path d="M6 2v4.5M6 9v.01" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>error</span>`
	case StatusRunning:
		return `<span class="inline-flex items-center gap-1.5 font-mono text-[10px] font-semibold uppercase tracking-wide text-live"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span>running</span>`
	default:
		return `<span class="font-mono text-[10px] font-semibold uppercase tracking-wide text-faint">unparsed</span>`
	}
}
