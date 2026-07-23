package main

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

// All model-facing prompts and human-facing outbound text live in ai/prompts/
// and are compiled into the binary, so a release is still one self-contained
// file that reads nothing from disk at runtime.
//
// The embed pattern deliberately mirrors ParseFS's glob below. Neither one
// descends, and the directory is flat on purpose: template.ParseFS names each
// template by its base filename, so two files with the same name in different
// subdirectories would silently shadow one another. A nested file would ship
// unparsed and fail only as a runtime panic, so TestEveryPromptFileOnDiskIsParsed
// enforces flatness against the real directory at build time.
//
//go:embed ai/prompts/*.md.tmpl
var promptFS embed.FS

// missingkey=error is load-bearing, not incidental: without it a typo'd
// placeholder renders as the literal "<no value>" inside a prompt that then
// gets sent to Claude — a silent, expensive failure. With it, the same typo is
// a loud render error that the tests in prompts_test.go catch.
var prompts = template.Must(
	template.New("prompts").
		Option("missingkey=error").
		ParseFS(promptFS, "ai/prompts/*.md.tmpl"),
)

// mustRender executes a named template and trims the file's trailing newline —
// editors end files with one, the string literals this replaced did not.
//
// It panics because a render failure is a static defect (unknown template name,
// missing key), not a runtime condition: prompts_test.go renders every template
// in the embedded FS, so such a defect fails the build instead of reaching a
// running daemon.
func mustRender(name string, data map[string]any) string {
	var buf bytes.Buffer
	if err := prompts.ExecuteTemplate(&buf, name, data); err != nil {
		panic(fmt.Sprintf("render prompt %q: %v", name, err))
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// promptData seeds template data with every sentinel constant. The sentinels are
// never written as literal text in a .tmpl file: the same constants drive the
// parsers in confidence.go, done.go, and pipeline_feature.go, and hardcoding
// them in the prompts would let the instruction and the parser drift apart.
func promptData() map[string]any {
	return map[string]any{
		"ConfidenceSentinel":  confidenceSentinel,
		"SpecReadySentinel":   specReadySentinel,
		"ReadySentinel":       readySentinel,
		"AlreadyDoneSentinel": alreadyDoneSentinel,
		"DoneConfirmSentinel": doneConfirmSentinel,
	}
}
