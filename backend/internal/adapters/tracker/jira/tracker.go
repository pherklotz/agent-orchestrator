// Package jira implements the ports.Tracker outbound port for Jira by
// shelling out to the jira CLI (github.com/ankitpokhrel/jira-cli). The CLI
// owns server selection and credentials (`jira init`, JIRA_API_TOKEN), so the
// adapter carries no auth of its own.
//
//   - Get runs `jira issue view <KEY> --raw`.
//   - List runs `jira issue list --raw` scoped to a project key, then hydrates
//     each hit through Get: the list payload drops status categories, emails,
//     and the issue URL that state mapping and assignee matching need.
//   - Preflight runs `jira me`.
//
// # State mapping
//
// Jira statuses are workflow-specific, so the adapter maps the fixed status
// category instead of the status name:
//
//   - new           -> open
//   - indeterminate -> in_progress
//   - done          -> done
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Sentinel errors.
var (
	ErrNotInstalled  = errors.New("jira tracker: jira CLI not found on PATH")
	ErrWrongProvider = errors.New("jira tracker: id is not a jira tracker id")
	ErrBadID         = errors.New("jira tracker: malformed issue key or project key")
)

// listLimit is jira-cli's per-call maximum.
// ponytail: no pagination past 100 issues; intake is assignee-scoped, so one page covers it.
const listLimit = 100

var (
	issueKeyRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)
	projectKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// isIssueKey reports whether s looks like a Jira issue key such as "PROJ-123".
func isIssueKey(s string) bool { return issueKeyRe.MatchString(s) }

// Runner runs the jira CLI with args and returns its stdout. Errors should
// carry the CLI's stderr so callers can classify them.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Tracker is the jira-cli backed tracker.
type Tracker struct {
	run Runner
}

// New returns a Tracker that runs the jira CLI from PATH. It fails with
// ErrNotInstalled when the CLI is missing so daemon wiring can skip Jira.
func New() (*Tracker, error) {
	if _, err := exec.LookPath("jira"); err != nil {
		return nil, ErrNotInstalled
	}
	return NewWithRunner(execRunner), nil
}

// NewWithRunner returns a Tracker that uses run instead of the real CLI.
func NewWithRunner(run Runner) *Tracker { return &Tracker{run: run} }

func execRunner(ctx context.Context, args ...string) ([]byte, error) {
	out, err := aoprocess.CommandContext(ctx, "jira", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("jira %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("jira %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return out, nil
}

// Get returns one issue by key.
func (t *Tracker) Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error) {
	if id.Provider != domain.TrackerProviderJira {
		return domain.Issue{}, ErrWrongProvider
	}
	key := strings.ToUpper(strings.TrimSpace(id.Native))
	if !isIssueKey(key) {
		return domain.Issue{}, fmt.Errorf("%w: %q", ErrBadID, id.Native)
	}
	out, err := t.run(ctx, "issue", "view", key, "--raw")
	if err != nil {
		return domain.Issue{}, err
	}
	var raw rawIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return domain.Issue{}, fmt.Errorf("jira tracker: decode issue %s: %w", key, err)
	}
	return raw.normalize(), nil
}

// List returns issues in the Jira project repo.Native, filtered by filter.
func (t *Tracker) List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	if repo.Provider != domain.TrackerProviderJira {
		return nil, ErrWrongProvider
	}
	project := strings.TrimSpace(repo.Native)
	if !projectKeyRe.MatchString(project) {
		return nil, fmt.Errorf("%w: %q", ErrBadID, repo.Native)
	}
	limit := listLimit
	if filter.Limit > 0 && filter.Limit < limit {
		limit = filter.Limit
	}
	args := append([]string{"issue", "list", "--raw", "--project", project, "--paginate", fmt.Sprintf("0:%d", limit)}, listFilterArgs(filter)...)
	out, err := t.run(ctx, args...)
	if err != nil {
		// jira-cli exits non-zero instead of printing [] when nothing matches.
		if strings.Contains(err.Error(), "No result found") {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	var hits []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("jira tracker: decode issue list: %w", err)
	}
	issues := make([]domain.Issue, 0, len(hits))
	for _, hit := range hits {
		issue, err := t.Get(ctx, domain.TrackerID{Provider: domain.TrackerProviderJira, Native: hit.Key})
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// Preflight verifies the CLI is configured and authenticated.
func (t *Tracker) Preflight(ctx context.Context) error {
	_, err := t.run(ctx, "me")
	return err
}

func listFilterArgs(filter domain.ListFilter) []string {
	var jql, args []string
	switch filter.State {
	case domain.ListOpen:
		jql = append(jql, "statusCategory != Done")
	case domain.ListClosed:
		jql = append(jql, "statusCategory = Done")
	}
	for _, label := range filter.Labels {
		jql = append(jql, "labels = "+quoteJQL(label))
	}
	switch assignee := strings.TrimSpace(filter.Assignee); {
	case assignee == "":
	case assignee == "*":
		jql = append(jql, "assignee is not EMPTY")
	case strings.EqualFold(assignee, "none"):
		jql = append(jql, "assignee is EMPTY")
	default:
		// jira-cli resolves emails and display names to the right account id.
		args = append(args, "--assignee", assignee)
	}
	if len(jql) > 0 {
		args = append(args, "--jql", strings.Join(jql, " AND "))
	}
	return args
}

func quoteJQL(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

type rawUser struct {
	AccountID    string `json:"accountId"`
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type rawIssue struct {
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		Assignee    *rawUser        `json:"assignee"`
		Status      struct {
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
	} `json:"fields"`
}

func (r rawIssue) normalize() domain.Issue {
	issue := domain.Issue{
		ID:     domain.TrackerID{Provider: domain.TrackerProviderJira, Native: r.Key},
		Title:  r.Fields.Summary,
		Body:   descriptionText(r.Fields.Description),
		State:  mapState(r.Fields.Status.StatusCategory.Key),
		Labels: r.Fields.Labels,
	}
	if base, _, ok := strings.Cut(r.Self, "/rest/"); ok && base != "" {
		issue.URL = base + "/browse/" + r.Key
	}
	// Every identity form is listed so intake's assignee match accepts an
	// email, display name, Server username, or Cloud account id.
	if a := r.Fields.Assignee; a != nil {
		for _, v := range []string{a.EmailAddress, a.DisplayName, a.Name, a.AccountID} {
			if v != "" {
				issue.Assignees = append(issue.Assignees, v)
			}
		}
	}
	return issue
}

func mapState(category string) domain.NormalizedIssueState {
	switch category {
	case "indeterminate":
		return domain.IssueInProgress
	case "done":
		return domain.IssueDone
	default:
		return domain.IssueOpen
	}
}

// descriptionText handles both API shapes jira-cli returns: a plain string
// (Server / API v2) and an Atlassian Document Format tree (Cloud / API v3).
func descriptionText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	var b strings.Builder
	adfText(doc, &b)
	return strings.TrimSpace(b.String())
}

func adfText(node any, b *strings.Builder) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if text, ok := m["text"].(string); ok {
		b.WriteString(text)
	}
	if children, ok := m["content"].([]any); ok {
		for _, child := range children {
			adfText(child, b)
		}
	}
	switch m["type"] {
	case "paragraph", "heading", "listItem", "codeBlock", "blockquote", "hardBreak", "rule":
		b.WriteByte('\n')
	}
}

var _ ports.Tracker = (*Tracker)(nil)
