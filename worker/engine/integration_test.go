//go:build integration

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
)

// Requires a real `claude` binary on PATH and API access.
// Run with: go test -tags integration -run TestIntegrationTriage -v
func TestIntegrationTriage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := infra.NewClaude(infra.ExecRunner{}, "", "")
	issues := []shared.Issue{
		{Number: 1, Title: "Typo in README", Body: "The word 'teh' appears in the intro."},
		{Number: 2, Title: "Design a plugin system", Body: "We need extensibility for third parties."},
	}
	dec, err := Triage(ctx, c, shared.ModelConfig{Model: "sonnet"}, t.TempDir(), issues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 1 && dec.IssueNumber != 2 {
		t.Errorf("dec = %+v", dec)
	}
	t.Logf("triage picked #%d (%s): %s", dec.IssueNumber, dec.Kind, dec.Reason)
}
