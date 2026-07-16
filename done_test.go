package main

import "testing"

func TestParseAlreadyDone(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOK   bool
		wantReas string
	}{
		{"absent", "just some brainstorming text", false, ""},
		{"basic", "PIPELINE_ALREADY_DONE: export lives in exporter.go", true, "export lives in exporter.go"},
		{"embedded in output", "I checked the code.\nPIPELINE_ALREADY_DONE: already there\nbye", true, "already there"},
		{"trims spaces", "PIPELINE_ALREADY_DONE:    extra spaces   ", true, "extra spaces"},
		{"missing reason", "PIPELINE_ALREADY_DONE:", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := parseAlreadyDone(tc.in)
			if ok != tc.wantOK || reason != tc.wantReas {
				t.Errorf("parseAlreadyDone(%q) = (%q, %v), want (%q, %v)", tc.in, reason, ok, tc.wantReas, tc.wantOK)
			}
		})
	}
}

func TestAlreadyDoneErrorMessage(t *testing.T) {
	e := &alreadyDoneError{reason: "already in foo.go"}
	if e.Error() != "already implemented: already in foo.go" {
		t.Errorf("Error() = %q", e.Error())
	}
}
