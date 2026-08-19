package main

import "testing"

func TestWrappedAndBareCommand(t *testing.T) {
	got := wrappedCommand("/usr/local/bin/loope", "/path/to/real-statusline.sh")
	want := `sh -c 'tee >(/usr/local/bin/loope claude-usage-hook) | /path/to/real-statusline.sh'`
	if got != want {
		t.Errorf("wrappedCommand = %q, want %q", got, want)
	}

	got = bareCommand("/usr/local/bin/loope")
	want = `sh -c 'tee >(/usr/local/bin/loope claude-usage-hook) >/dev/null'`
	if got != want {
		t.Errorf("bareCommand = %q, want %q", got, want)
	}
}

func TestMatchOursBare(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	isOurs, isWrapped, original := matchOurs(bareCommand(loopePath), loopePath)
	if !isOurs || isWrapped || original != "" {
		t.Fatalf("matchOurs(bare) = (%v, %v, %q), want (true, false, \"\")", isOurs, isWrapped, original)
	}
}

func TestMatchOursWrapped(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	cmd := wrappedCommand(loopePath, "/path/to/real-statusline.sh")
	isOurs, isWrapped, original := matchOurs(cmd, loopePath)
	if !isOurs || !isWrapped || original != "/path/to/real-statusline.sh" {
		t.Fatalf("matchOurs(wrapped) = (%v, %v, %q), want (true, true, %q)",
			isOurs, isWrapped, original, "/path/to/real-statusline.sh")
	}
}

func TestMatchOursForeignCommand(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	isOurs, _, _ := matchOurs("/some/other/statusline.sh", loopePath)
	if isOurs {
		t.Fatal("matchOurs: unrelated command must not match")
	}
}

func TestMatchOursDifferentLoopePath(t *testing.T) {
	// A command wrapped for a different loope binary path must not match —
	// this is what makes remove-after-move fail safely (edit manually)
	// rather than silently discarding an unrelated statusLine.
	cmd := wrappedCommand("/old/path/loope", "/path/to/real-statusline.sh")
	isOurs, _, _ := matchOurs(cmd, "/usr/local/bin/loope")
	if isOurs {
		t.Fatal("matchOurs: command wrapped for a different loope path must not match")
	}
}
