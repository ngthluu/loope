package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Label struct {
	Name string `json:"name"`
}

type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Labels []Label `json:"labels"`
}

type GitHub struct {
	runner   Runner
	repoPath string
	slug     string
	state    StateLabels
	eligible string
	retry    RetryPolicy
}

func NewGitHub(r Runner, cfg *Config) *GitHub {
	return &GitHub{runner: r, repoPath: cfg.RepoPath, slug: cfg.RepoSlug,
		state: cfg.StateLabels, eligible: cfg.EligibleLabel, retry: cfg.GitHubRetry.policy()}
}

func (g *GitHub) gh(ctx context.Context, args ...string) (string, error) {
	var stdout string
	err := g.retry.do(ctx, isTransientGitHubError, func() error {
		out, stderr, e := g.runner.Run(ctx, g.repoPath, nil, "", "gh", args...)
		if e != nil {
			return fmt.Errorf("gh %s: %w (stderr: %s)", strings.Join(args[:min(2, len(args))], " "), e, tail(stderr, 300))
		}
		stdout = out
		return nil
	})
	return stdout, err
}

func (g *GitHub) ListEligibleIssues(ctx context.Context, label string) ([]Issue, error) {
	out, err := g.gh(ctx, "issue", "list", "--repo", g.slug, "--label", label,
		"--state", "open", "--limit", "50", "--json", "number,title,body,labels")
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	var eligible []Issue
	for _, is := range issues {
		if !g.hasStateLabel(is) {
			eligible = append(eligible, is)
		}
	}
	return eligible, nil
}

// ListIssuesWithLabel returns every open issue carrying label, with no state
// filtering — unlike ListEligibleIssues, which drops issues already in a state.
// Used by the resume scan (rework label) and the startup orphan sweep
// (wip label), where the state label IS the query.
//
// State labels (ai-wip/ai-rework/…) are shared by everyone running the tool
// against this repo, while the eligible label is per-instance. To avoid one
// user's loop resuming or sweeping another's issues, the query also requires
// the eligible label (gh treats repeated --label as AND), so only issues
// carrying BOTH label and this instance's eligible label are returned. The
// eligible label rides along on an issue for its whole lifecycle (only state
// labels are added/swapped/removed), so this never hides our own work.
func (g *GitHub) ListIssuesWithLabel(ctx context.Context, label string) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", g.slug, "--label", label}
	if g.eligible != "" {
		args = append(args, "--label", g.eligible)
	}
	args = append(args, "--state", "open", "--limit", "100", "--json", "number,title,body,labels")
	out, err := g.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	return issues, nil
}

func (g *GitHub) hasStateLabel(is Issue) bool {
	for _, l := range is.Labels {
		if l.Name == g.state.WIP || l.Name == g.state.Done ||
			l.Name == g.state.Rework || l.Name == g.state.NeedsInfo || l.Name == g.state.Stopped {
			return true
		}
	}
	return false
}

func (g *GitHub) AddLabel(ctx context.Context, num int, label string) error {
	_, err := g.gh(ctx, "issue", "edit", strconv.Itoa(num), "--repo", g.slug, "--add-label", label)
	return err
}

func (g *GitHub) RemoveLabel(ctx context.Context, num int, label string) error {
	_, err := g.gh(ctx, "issue", "edit", strconv.Itoa(num), "--repo", g.slug, "--remove-label", label)
	return err
}

// SwapLabels atomically removes one label and adds another via a single
// `gh issue edit` call, so a state label is never dropped without its
// replacement being applied (unlike a separate RemoveLabel+AddLabel pair).
func (g *GitHub) SwapLabels(ctx context.Context, num int, remove, add string) error {
	_, err := g.gh(ctx, "issue", "edit", strconv.Itoa(num), "--repo", g.slug,
		"--remove-label", remove, "--add-label", add)
	return err
}

func (g *GitHub) Comment(ctx context.Context, num int, body string) error {
	_, err := g.gh(ctx, "issue", "comment", strconv.Itoa(num), "--repo", g.slug, "--body", body)
	return err
}

func (g *GitHub) CloseIssue(ctx context.Context, num int) error {
	_, err := g.gh(ctx, "issue", "close", strconv.Itoa(num), "--repo", g.slug)
	return err
}

func (g *GitHub) FetchIssueContent(ctx context.Context, num int) (string, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(num), "--repo", g.slug,
		"--json", "title,body,comments")
	if err != nil {
		return "", err
	}
	var detail struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &detail); err != nil {
		return "", fmt.Errorf("parse issue view: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (#%d)\n\n%s\n", detail.Title, num, detail.Body)
	// The daemon comments on its own issues (pickup, park + error dump, PR link,
	// ...), so without this filter every re-run feeds the model a growing
	// transcript of the previous runs' status chatter as if it were part of the
	// report. Only human-written comments — and the bot's needs-info questions,
	// which the human's answer refers back to — are context.
	var comments []string
	for _, c := range detail.Comments {
		if isBotStatusComment(c.Body) {
			continue
		}
		comments = append(comments, fmt.Sprintf("\n@%s: %s\n", c.Author.Login, c.Body))
	}
	if len(comments) > 0 {
		b.WriteString("\n## Comments\n")
		for _, c := range comments {
			b.WriteString(c)
		}
	}
	return b.String(), nil
}

// IssueTitle returns just the issue's title, used by the rework command to build
// the PR title without re-fetching the full body/comments.
func (g *GitHub) IssueTitle(ctx context.Context, num int) (string, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(num), "--repo", g.slug, "--json", "title")
	if err != nil {
		return "", err
	}
	var v struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse issue title: %w", err)
	}
	return v.Title, nil
}

// UATSurfaces returns every text on the issue that could carry the UAT marker,
// for the UAT step's idempotency check: each comment, plus the body, where the
// checklist was published before it moved to a comment. One `gh issue view`
// covers both, and the body entry is what keeps an issue that already has a
// body checklist from gaining a duplicate comment.
func (g *GitHub) UATSurfaces(ctx context.Context, n int) ([]string, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(n), "--repo", g.slug, "--json", "body,comments")
	if err != nil {
		return nil, err
	}
	var v struct {
		Body     string `json:"body"`
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parse issue view: %w", err)
	}
	surfaces := []string{v.Body}
	for _, c := range v.Comments {
		surfaces = append(surfaces, c.Body)
	}
	return surfaces, nil
}

func (g *GitHub) CreatePR(ctx context.Context, branch, title, body string) (string, error) {
	out, err := g.gh(ctx, "pr", "create", "--repo", g.slug, "--head", branch,
		"--title", title, "--body", body)
	if err != nil {
		// A PR for this head branch may already exist: a prior run pushed the
		// branch and opened the PR but didn't reach the Done state (interrupted,
		// or a best-effort label swap silently failed), so the issue was picked
		// up again. That is the desired end state, not a failure — recover the
		// existing PR's URL and treat it as success so the loop marks the issue
		// Done instead of Failed.
		if strings.Contains(err.Error(), "already exists") {
			if url, verr := g.existingPRURL(ctx, branch); verr == nil {
				return url, nil
			}
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// existingPRURL returns the URL of the open PR whose head is branch.
func (g *GitHub) existingPRURL(ctx context.Context, branch string) (string, error) {
	out, err := g.gh(ctx, "pr", "view", branch, "--repo", g.slug, "--json", "url")
	if err != nil {
		return "", err
	}
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse pr view: %w", err)
	}
	if v.URL == "" {
		return "", fmt.Errorf("pr view for %s returned no url", branch)
	}
	return v.URL, nil
}

// PRURLForBranch returns the URL of the PR whose head is branch, for backfilling
// the dashboard's pr cache on tickets shipped before the URL was persisted.
func (g *GitHub) PRURLForBranch(ctx context.Context, branch string) (string, error) {
	return g.existingPRURL(ctx, branch)
}

// PRNumberForBranch returns the number of the open PR whose head is branch,
// for CodeReview.Run to know where to post review findings.
func (g *GitHub) PRNumberForBranch(ctx context.Context, branch string) (int, error) {
	out, err := g.gh(ctx, "pr", "view", branch, "--repo", g.slug, "--json", "number")
	if err != nil {
		return 0, err
	}
	var v struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return 0, fmt.Errorf("parse pr view: %w", err)
	}
	return v.Number, nil
}

// ReviewComment posts a top-level PR review comment via `gh pr review
// --comment`, distinct from Comment (an issue-style comment): the post-ship
// code review loop's findings belong on the PR, not the issue.
func (g *GitHub) ReviewComment(ctx context.Context, prNumber int, body string) error {
	_, err := g.gh(ctx, "pr", "review", strconv.Itoa(prNumber), "--repo", g.slug, "--comment", "--body", body)
	return err
}
