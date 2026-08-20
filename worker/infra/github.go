package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/ngthluu/loope/worker/shared"
)

// GitHub is the gh-CLI adapter behind shared.CodeHost.
type GitHub struct {
	runner   shared.Runner
	repoPath string
	slug     string
	state    shared.StateLabels
	eligible string
	retry    shared.RetryPolicy
}

var _ shared.CodeHost = (*GitHub)(nil)

func NewGitHub(r shared.Runner, cfg *shared.Config) *GitHub {
	return &GitHub{runner: r, repoPath: cfg.RepoPath, slug: cfg.RepoSlug,
		state: cfg.StateLabels, eligible: cfg.EligibleLabel, retry: cfg.GitHubRetry.Policy()}
}

func (g *GitHub) gh(ctx context.Context, args ...string) (string, error) {
	return g.ghRetrying(ctx, shared.IsTransientGitHubError, args...)
}

// ghWrite runs a non-idempotent gh command (posting a comment/review). It
// retries only when the failure proves the request never reached GitHub:
// retrying on a post-send-ambiguous failure ("timeout", "unexpected eof",
// 5xx — the server may well have applied the write before the connection
// died) would duplicate the comment on the issue/PR.
func (g *GitHub) ghWrite(ctx context.Context, args ...string) (string, error) {
	return g.ghRetrying(ctx, isSafeToRetryWrite, args...)
}

// isSafeToRetryWrite reports whether err is a failure that happened before the
// request was sent — connection never established, name resolution failed, or
// GitHub rejected it up front with a rate limit (429 / secondary rate limit,
// which is returned without applying the write). Those are the only transient
// signatures under which re-sending a write cannot duplicate it. Everything
// else that shared.IsTransientGitHubError would retry (timeouts, connection
// reset, unexpected EOF, 5xx) is ambiguous and deliberately excluded.
func isSafeToRetryWrite(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range preSendTransientSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

var preSendTransientSignatures = []string{
	"rate limit", "abuse detection", "submitted too quickly", "secondary rate limit",
	"http 429", "returned error: 429", "status 429",
	"connection refused", "could not resolve host", "couldn't connect",
	"network is unreachable", "temporary failure in name resolution",
}

func (g *GitHub) ghRetrying(ctx context.Context, isRetryable func(error) bool, args ...string) (string, error) {
	var stdout string
	err := g.retry.Do(ctx, isRetryable, func() error {
		out, stderr, e := g.runner.Run(ctx, g.repoPath, nil, "", "gh", args...)
		if e != nil {
			return fmt.Errorf("gh %s: %w (stderr: %s)", strings.Join(args[:min(2, len(args))], " "), e, shared.Tail(stderr, 300))
		}
		stdout = out
		return nil
	})
	return stdout, err
}

// eligibleListLimit caps one `gh issue list` page for the eligible scan. State
// exclusion is pushed server-side (see ListEligibleIssues) so the page is
// spent on genuinely eligible issues, not ones the client would drop anyway.
const eligibleListLimit = 200

func (g *GitHub) ListEligibleIssues(ctx context.Context, label string) ([]shared.Issue, error) {
	args := []string{"issue", "list", "--repo", g.slug, "--label", label,
		"--state", "open", "--limit", strconv.Itoa(eligibleListLimit)}
	// Exclude issues already in a state on the server: with only client-side
	// filtering, once more than --limit open issues carry the eligible label the
	// page fills with in-state issues and older eligible ones silently never
	// surface. The client-side filter below stays as belt-and-braces.
	var excl []string
	for _, name := range g.state.All() {
		if name != "" {
			excl = append(excl, "-label:"+name)
		}
	}
	if len(excl) > 0 {
		args = append(args, "--search", strings.Join(excl, " "))
	}
	args = append(args, "--json", "number,title,body,labels")
	out, err := g.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []shared.Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	if len(issues) >= eligibleListLimit {
		log.Printf("github: eligible issue list for %q hit the %d-item limit; older eligible issues may be missing this cycle", label, eligibleListLimit)
	}
	var eligible []shared.Issue
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
func (g *GitHub) ListIssuesWithLabel(ctx context.Context, label string) ([]shared.Issue, error) {
	args := []string{"issue", "list", "--repo", g.slug, "--label", label}
	if g.eligible != "" {
		args = append(args, "--label", g.eligible)
	}
	args = append(args, "--state", "open", "--limit", "100", "--json", "number,title,body,labels")
	out, err := g.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []shared.Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	return issues, nil
}

func (g *GitHub) hasStateLabel(is shared.Issue) bool {
	return g.state.Current(is.Labels) != ""
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

// Comment posts an issue comment. Non-idempotent, so it goes through ghWrite:
// a retry after an ambiguous failure could post the same comment twice.
func (g *GitHub) Comment(ctx context.Context, num int, body string) error {
	_, err := g.ghWrite(ctx, "issue", "comment", strconv.Itoa(num), "--repo", g.slug, "--body", body)
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
		if shared.IsBotStatusComment(c.Body) {
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
			if url, _, verr := g.prView(ctx, branch); verr == nil {
				return url, nil
			}
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// prView looks up the open PR whose head is branch and returns its URL and
// number in one `gh pr view` call — the single seam behind PRURLForBranch,
// PRNumberForBranch and CreatePR's existing-PR recovery.
func (g *GitHub) prView(ctx context.Context, branch string) (url string, number int, err error) {
	out, err := g.gh(ctx, "pr", "view", branch, "--repo", g.slug, "--json", "url,number")
	if err != nil {
		return "", 0, err
	}
	var v struct {
		URL    string `json:"url"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", 0, fmt.Errorf("parse pr view: %w", err)
	}
	if v.URL == "" && v.Number == 0 {
		return "", 0, fmt.Errorf("pr view for %s returned no pr", branch)
	}
	return v.URL, v.Number, nil
}

// PRURLForBranch returns the URL of the PR whose head is branch, for backfilling
// the dashboard's pr cache on tickets shipped before the URL was persisted.
func (g *GitHub) PRURLForBranch(ctx context.Context, branch string) (string, error) {
	url, _, err := g.prView(ctx, branch)
	return url, err
}

// PRNumberForBranch returns the number of the open PR whose head is branch,
// for CodeReview.Run to know where to post review findings.
func (g *GitHub) PRNumberForBranch(ctx context.Context, branch string) (int, error) {
	_, n, err := g.prView(ctx, branch)
	return n, err
}

// ReviewComment posts a top-level PR review comment via `gh pr review
// --comment`, distinct from Comment (an issue-style comment): the post-ship
// code review loop's findings belong on the PR, not the issue. Non-idempotent
// like Comment, so it goes through ghWrite.
func (g *GitHub) ReviewComment(ctx context.Context, prNumber int, body string) error {
	_, err := g.ghWrite(ctx, "pr", "review", strconv.Itoa(prNumber), "--repo", g.slug, "--comment", "--body", body)
	return err
}

// NewGitHubWithRetry is NewGitHub with an explicit retry policy — used by
// tests, which need near-instant backoff instead of the config's seconds.
func NewGitHubWithRetry(r shared.Runner, cfg *shared.Config, retry shared.RetryPolicy) *GitHub {
	g := NewGitHub(r, cfg)
	g.retry = retry
	return g
}
