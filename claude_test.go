package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallBuildsHeadlessArgs(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	res, err := c.Call(context.Background(), ClaudeCall{
		Dir:             "/wt",
		Label:           "brainstorm-0",
		Prompt:          "hello",
		Model:           ModelConfig{Model: "opus", Effort: "high", MaxBudgetUSD: 15, MaxTurns: 100},
		PermissionMode:  permissionModeAuto,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "s1" || res.Result != "ok" {
		t.Errorf("res = %+v", res)
	}
	call := f.calls[0]
	if call.name != "claude" || call.dir != "/wt" {
		t.Errorf("call = %+v", call)
	}
	for _, want := range []string{"-p", "--permission-mode"} {
		if !hasArg(call.args, want) {
			t.Errorf("args missing %q: %v", want, call.args)
		}
	}
	if got := argAfter(call.args, "--output-format"); got != "stream-json" {
		t.Errorf("--output-format = %q", got)
	}
	if !hasArg(call.args, "--verbose") {
		t.Errorf("stream-json requires --verbose: %v", call.args)
	}
	if got := argAfter(call.args, "--model"); got != "opus" {
		t.Errorf("--model = %q", got)
	}
	if got := argAfter(call.args, "--max-budget-usd"); got != "15" {
		t.Errorf("--max-budget-usd = %q", got)
	}
	if got := argAfter(call.args, "--max-turns"); got != "100" {
		t.Errorf("--max-turns = %q", got)
	}
	if got := argAfter(call.args, "--disallowedTools"); got != "AskUserQuestion" {
		t.Errorf("--disallowedTools = %q", got)
	}
	if call.stdin != "hello" {
		t.Errorf("prompt must be fed via stdin, got stdin=%q args=%v", call.stdin, call.args)
	}
	if hasArg(call.args, "hello") {
		t.Errorf("prompt must not appear in argv, got %v", call.args)
	}
}

// TestCallFeedsPromptViaStdin pins the fix for the brainstorm-0 failure:
// claude's --disallowedTools is variadic and greedily consumes every following
// arg until the next flag. When the prompt was a positional argument after it,
// the prompt was swallowed word-by-word ("Permission deny rule ... matches no
// known tool") and claude errored with "Input must be provided". Feeding the
// prompt on stdin keeps it out of argv entirely.
func TestCallFeedsPromptViaStdin(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	prompt := "print PIPELINE_READY on its own line"
	_, err := c.Call(context.Background(), ClaudeCall{
		Label:           "brainstorm-0",
		Prompt:          prompt,
		Model:           ModelConfig{Model: "opus"},
		PermissionMode:  permissionModeAuto,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[0].stdin != prompt {
		t.Errorf("stdin = %q, want the prompt", f.calls[0].stdin)
	}
	for _, a := range f.calls[0].args {
		if strings.Contains(a, "PIPELINE_READY") {
			t.Errorf("prompt words must not leak into argv, got %v", f.calls[0].args)
		}
	}
}

// TestCallSavesPromptMarkdown verifies the exact prompt fed to claude is also
// persisted as <seq>-<label>.prompt.md alongside the JSON postmortem.
func TestCallSavesPromptMarkdown(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f, logDir: dir}
	prompt := "/superpowers:brainstorming build a thing\n\nCommit and print DONE."
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "brainstorm-0", Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "001-brainstorm-0.prompt.md"))
	if err != nil {
		t.Fatalf("prompt file not written: %v", err)
	}
	if string(data) != prompt {
		t.Errorf("prompt file = %q, want %q", string(data), prompt)
	}
	// prompt.md and json share the sequence number.
	if _, err := os.Stat(filepath.Join(dir, "001-brainstorm-0.json")); err != nil {
		t.Errorf("json log not written with matching seq: %v", err)
	}
}

func TestCallIncludesEffort(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	_, err := c.Call(context.Background(), ClaudeCall{Prompt: "x", Model: ModelConfig{Model: "opus", Effort: "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(f.calls[0].args, "--effort"); got != "high" {
		t.Errorf("--effort = %q", got)
	}
}

func TestCallSetsConfigDirEnv(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f, configDir: "/home/u/.claude-personal"}
	if _, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if !hasArg(f.calls[0].env, "CLAUDE_CONFIG_DIR=/home/u/.claude-personal") {
		t.Errorf("env = %v, want CLAUDE_CONFIG_DIR set", f.calls[0].env)
	}
}

func TestCallOmitsConfigDirEnvWhenUnset(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	if _, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].env != nil {
		t.Errorf("env = %v, want nil when configDir unset", f.calls[0].env)
	}
}

func TestCallResumePassesSessionID(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	_, err := c.Call(context.Background(), ClaudeCall{Prompt: "answer", Resume: "s1", Model: ModelConfig{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(f.calls[0].args, "--resume"); got != "s1" {
		t.Errorf("--resume = %q", got)
	}
}

func TestCallErrorOnNonZeroExit(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stderr: "boom", err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	if _, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"}); err == nil {
		t.Error("want error, got nil")
	}
}

func TestCallErrorOnIsError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{"result": "bad", "session_id": "s", "is_error": true}`}}}
	c := &Claude{runner: f}
	if _, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"}); err == nil {
		t.Error("want error on is_error, got nil")
	}
}

// TestCallReturnsResultOnIsError pins the contract that lets a failed run be
// resumed: when claude reports is_error but the JSON parsed and carries a
// session id (e.g. a 429 session limit), Call returns the parsed result
// alongside the error so callers can still record the session for -rework.
func TestCallReturnsResultOnIsError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeErrorJSON("hit your session limit", "sess-429")}}}
	c := &Claude{runner: f}
	res, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"})
	if err == nil {
		t.Fatal("want error on is_error session")
	}
	if res == nil {
		t.Fatal("want non-nil result on is_error so the session can be recovered, got nil")
	}
	if res.SessionID != "sess-429" {
		t.Errorf("res.SessionID = %q, want sess-429", res.SessionID)
	}
}

// TestCallReturnsNilOnTransportError verifies the contract holds only when the
// JSON parsed: a transport/exec failure has no session to hand back, so the
// result stays nil.
func TestCallReturnsNilOnTransportError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stderr: "boom", err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	res, err := c.Call(context.Background(), ClaudeCall{Prompt: "x"})
	if err == nil {
		t.Fatal("want error on transport failure")
	}
	if res != nil {
		t.Errorf("want nil result on transport failure, got %+v", res)
	}
}

func TestCallSavesLog(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "triage", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "001-triage.json"))
	if err != nil {
		t.Fatalf("json log not written: %v", err)
	}
	if !strings.Contains(string(data), `"session_id"`) {
		t.Error("log should contain raw claude JSON")
	}
}

// TestCallSavesOutputMarkdown verifies the model's result text is persisted as
// <seq>-<label>.output.md alongside the prompt and JSON.
func TestCallSavesOutputMarkdown(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("the model reply", "s1")}}}
	c := &Claude{runner: f, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "triage", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "001-triage.output.md"))
	if err != nil {
		t.Fatalf("output file not written: %v", err)
	}
	if string(data) != "the model reply" {
		t.Errorf("output file = %q, want the result text", string(data))
	}
}

// TestCallSavesOutputOnError verifies the output.md is written even when the
// session is is_error, capturing the error message for postmortems.
func TestCallSavesOutputOnError(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: `{"result": "session blew up", "session_id": "s", "is_error": true}`}}}
	c := &Claude{runner: f, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "debug", Prompt: "x"}); err == nil {
		t.Fatal("want error on is_error session")
	}
	data, err := os.ReadFile(filepath.Join(dir, "001-debug.output.md"))
	if err != nil {
		t.Fatalf("output file not written on error session: %v", err)
	}
	if string(data) != "session blew up" {
		t.Errorf("output file = %q, want the error message", string(data))
	}
}

func TestCallLogsDoNotOverwriteAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	f1 := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c1 := &Claude{runner: f1, logDir: dir}
	if _, err := c1.Call(context.Background(), ClaudeCall{Label: "triage", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}

	f2 := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s2")}}}
	c2 := &Claude{runner: f2, logDir: dir}
	if _, err := c2.Call(context.Background(), ClaudeCall{Label: "triage", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	var jsons int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsons++
		}
	}
	if jsons != 2 {
		t.Fatalf("want 2 json logs, got %d: %v", jsons, entries)
	}
}

func TestTail(t *testing.T) {
	if got := tail("abcdef", 3); got != "def" {
		t.Errorf("tail = %q", got)
	}
	if got := tail("ab", 10); got != "ab" {
		t.Errorf("tail = %q", got)
	}
}

func TestRecordAndReadSession(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSession("sess-123", "feature")

	si, err := readSession(dir)
	if err != nil {
		t.Fatalf("readSession: %v", err)
	}
	if si.SessionID != "sess-123" || si.Kind != "feature" {
		t.Errorf("session = %+v, want sess-123/feature", si)
	}
}

func TestRecordSessionOverwritesAndSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSession("first", "bug")
	c.RecordSession("", "bug") // empty id must not overwrite
	c.RecordSession("second", "bug")

	si, err := readSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if si.SessionID != "second" {
		t.Errorf("session id = %q, want second (latest non-empty)", si.SessionID)
	}
}

func TestReadSessionMissing(t *testing.T) {
	if _, err := readSession(t.TempDir()); err == nil {
		t.Error("want error reading a missing session file")
	}
}

func TestClaudeResultParsesUsage(t *testing.T) {
	raw := `{"result":"ok","session_id":"s1","is_error":false,"total_cost_usd":2.63,
	  "num_turns":23,"duration_ms":206302,
	  "usage":{"input_tokens":18934,"cache_creation_input_tokens":161846,
	  "cache_read_input_tokens":1280292,"output_tokens":10933}}`
	var cr ClaudeResult
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	if cr.NumTurns != 23 || cr.DurationMS != 206302 {
		t.Fatalf("meta wrong: turns=%d dur=%d", cr.NumTurns, cr.DurationMS)
	}
	if cr.Usage.InputTokens != 18934 || cr.Usage.CacheCreationTokens != 161846 ||
		cr.Usage.CacheReadTokens != 1280292 || cr.Usage.OutputTokens != 10933 {
		t.Fatalf("usage wrong: %+v", cr.Usage)
	}
}

// TestClaudeResultParsesTerminalReason verifies the termination metadata used to
// classify parked issues is decoded from the result JSON.
func TestClaudeResultParsesTerminalReason(t *testing.T) {
	raw := `{"result":"","session_id":"s1","is_error":true,"terminal_reason":"max_turns","api_error_status":429}`
	var cr ClaudeResult
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	if cr.TerminalReason != "max_turns" || cr.APIErrorStatus != 429 {
		t.Errorf("parsed = %+v, want terminal_reason=max_turns api_error_status=429", cr)
	}
}

// TestCallErrorMessageIncludesTerminalReason pins that a max_turns cutoff — which
// carries an EMPTY result string — still produces an error that names the cause,
// so the park comment isn't blank.
func TestCallErrorMessageIncludesTerminalReason(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{"result":"","session_id":"s","is_error":true,"terminal_reason":"max_turns"}`}}}
	c := &Claude{runner: f}
	_, err := c.Call(context.Background(), ClaudeCall{Label: "execute", Prompt: "x"})
	if err == nil {
		t.Fatal("want error on is_error session")
	}
	if !strings.Contains(err.Error(), "max_turns") {
		t.Errorf("error must name the terminal reason even with an empty result, got %q", err.Error())
	}
}

func TestParseStreamResultUsesTerminalLine(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"s-init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"result","is_error":false,"result":"done","session_id":"s-final","total_cost_usd":0.5}` + "\n"
	res, line, err := parseStreamResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.SessionID != "s-final" || res.Result != "done" || res.CostUSD != 0.5 {
		t.Errorf("terminal result = %+v", res)
	}
	if !strings.Contains(line, `"result"`) {
		t.Errorf("terminal line = %q", line)
	}
}

// TestParseStreamResultSkipsTrailingHookResponse reproduces the real triage
// failure: an async SessionStart hook emits a hook_response system event *after*
// the terminal result event. The parser must return the result event, not the
// trailing hook line (which decodes to an empty, non-error result and made a
// good run look like "no JSON object in output").
func TestParseStreamResultSkipsTrailingHookResponse(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"s-init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"{\"issueNumber\": 843}","session_id":"s-final","total_cost_usd":0.5}
{"type":"system","subtype":"hook_response","hook_name":"SessionStart:startup","session_id":"s-hook"}` + "\n"
	res, line, err := parseStreamResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.SessionID != "s-final" || res.Result != `{"issueNumber": 843}` || res.CostUSD != 0.5 {
		t.Errorf("result = %+v, want the result event not the trailing hook", res)
	}
	if !strings.Contains(line, `"type":"result"`) {
		t.Errorf("terminal line = %q, want the result event", line)
	}
}

func TestParseStreamResultAcceptsSingleObject(t *testing.T) {
	res, _, err := parseStreamResult(claudeJSON("ok", "s1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.SessionID != "s1" || res.Result != "ok" {
		t.Errorf("single-object result = %+v", res)
	}
}

func TestParseStreamResultEmptyStream(t *testing.T) {
	// Empty string should return "empty stream output" error
	_, _, err := parseStreamResult("")
	if err == nil {
		t.Error("want error on empty stream, got nil")
	}
	if !strings.Contains(err.Error(), "empty stream output") {
		t.Errorf("error message = %q, want 'empty stream output'", err.Error())
	}

	// Whitespace-only should also return "empty stream output" error
	_, _, err = parseStreamResult("\n  \n")
	if err == nil {
		t.Error("want error on whitespace-only stream, got nil")
	}
	if !strings.Contains(err.Error(), "empty stream output") {
		t.Errorf("error message = %q, want 'empty stream output'", err.Error())
	}
}

func TestParseStreamResultMalformedTerminalLine(t *testing.T) {
	// Terminal line with invalid JSON should return unmarshal error
	raw := `{"type":"system","subtype":"init","session_id":"s1"}
{not valid json`
	_, _, err := parseStreamResult(raw)
	if err == nil {
		t.Error("want error on malformed terminal line, got nil")
	}
	// Error should be a JSON unmarshal error
	if !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error message = %q, want JSON unmarshal error", err.Error())
	}
}

func TestClaudeResultLegacyNoUsageIsZero(t *testing.T) {
	raw := `{"result":"ok","session_id":"s1","is_error":false,"total_cost_usd":0.5}`
	var cr ClaudeResult
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	if cr.NumTurns != 0 || cr.DurationMS != 0 || cr.Usage.InputTokens != 0 || cr.Usage.OutputTokens != 0 {
		t.Fatalf("legacy log should decode usage as zero, got turns=%d dur=%d usage=%+v",
			cr.NumTurns, cr.DurationMS, cr.Usage)
	}
}
