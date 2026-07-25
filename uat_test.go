package main

import (
	"strings"
	"testing"
)

func TestParseUATBeginAndEnd(t *testing.T) {
	got, ok := parseUAT("Here you go:\nUAT_BEGIN\n- [ ] click it\nUAT_END\nHope that helps!")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] click it" {
		t.Errorf("got %q, want the checklist with the surrounding prose stripped", got)
	}
}

// A session that opened the checklist but forgot the closing sentinel still
// publishes: take everything to the end of the result.
func TestParseUATBeginWithoutEnd(t *testing.T) {
	got, ok := parseUAT("UAT_BEGIN\n- [ ] one\n- [ ] two\n")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] one\n- [ ] two" {
		t.Errorf("got %q, want both items", got)
	}
}

func TestParseUATNoBegin(t *testing.T) {
	if got, ok := parseUAT("I could not find anything to verify."); ok {
		t.Errorf("want ok=false with no begin sentinel, got %q", got)
	}
}

// The bug route self-skips by emitting nothing at all, so an empty result must
// read as "nothing to publish".
func TestParseUATEmptyResult(t *testing.T) {
	if _, ok := parseUAT(""); ok {
		t.Error("want ok=false for an empty result")
	}
}

// Sentinels present but nothing between them is also "nothing to publish".
func TestParseUATEmptyContent(t *testing.T) {
	if got, ok := parseUAT("UAT_BEGIN\n\n   \nUAT_END"); ok {
		t.Errorf("want ok=false for an empty body between the sentinels, got %q", got)
	}
}

func TestUATSectionCarriesMarkerAndHeading(t *testing.T) {
	got := uatSection("- [ ] click it")
	if !strings.HasPrefix(got, uatMarker) {
		t.Errorf("section must lead with the idempotency marker:\n%s", got)
	}
	if !strings.Contains(got, "UAT checklist") || !strings.Contains(got, "- [ ] click it") {
		t.Errorf("section = %q", got)
	}
}
