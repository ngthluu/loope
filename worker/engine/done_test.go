package engine

import "testing"

func TestAlreadyDoneErrorMessage(t *testing.T) {
	e := &alreadyDoneError{reason: "already in foo.go"}
	if e.Error() != "already implemented: already in foo.go" {
		t.Errorf("Error() = %q", e.Error())
	}
}
