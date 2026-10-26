package jira

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const cloudIssue = `{
  "key": "PROJ-7",
  "self": "https://acme.atlassian.net/rest/api/3/issue/10007",
  "fields": {
    "summary": "Fix login",
    "description": {"type": "doc", "content": [
      {"type": "paragraph", "content": [{"type": "text", "text": "Users cannot "}, {"type": "text", "text": "log in."}]},
      {"type": "paragraph", "content": [{"type": "text", "text": "Second line"}]}
    ]},
    "labels": ["bug"],
    "assignee": {"accountId": "abc123", "displayName": "Peter H", "emailAddress": "peter@example.com"},
    "status": {"name": "To Do", "statusCategory": {"key": "new"}}
  }
}`

const serverIssue = `{
  "key": "OPS-2",
  "self": "https://jira.internal/rest/api/2/issue/2",
  "fields": {
    "summary": "Rotate keys",
    "description": "plain text body",
    "assignee": null,
    "status": {"name": "In Arbeit", "statusCategory": {"key": "indeterminate"}}
  }
}`

type fakeCLI struct {
	calls   [][]string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeCLI) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	for prefix, err := range f.errs {
		if strings.HasPrefix(key, prefix) {
			return nil, err
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(key, prefix) {
			return []byte(out), nil
		}
	}
	return nil, errors.New("unexpected call: " + key)
}

func TestGetNormalizesCloudAndServerIssues(t *testing.T) {
	cli := &fakeCLI{outputs: map[string]string{
		"issue view PROJ-7 --raw": cloudIssue,
		"issue view OPS-2 --raw":  serverIssue,
	}}
	tr := NewWithRunner(cli.run)

	got, err := tr.Get(context.Background(), domain.TrackerID{Provider: domain.TrackerProviderJira, Native: "PROJ-7"})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Issue{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderJira, Native: "PROJ-7"},
		Title:     "Fix login",
		Body:      "Users cannot log in.\nSecond line",
		State:     domain.IssueOpen,
		URL:       "https://acme.atlassian.net/browse/PROJ-7",
		Labels:    []string{"bug"},
		Assignees: []string{"peter@example.com", "Peter H", "abc123"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cloud issue:\n got %#v\nwant %#v", got, want)
	}

	got, err = tr.Get(context.Background(), domain.TrackerID{Provider: domain.TrackerProviderJira, Native: "OPS-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "plain text body" || got.State != domain.IssueInProgress || got.URL != "https://jira.internal/browse/OPS-2" || got.Assignees != nil {
		t.Fatalf("server issue = %#v", got)
	}
}

func TestGetRejectsBadInput(t *testing.T) {
	tr := NewWithRunner((&fakeCLI{}).run)
	if _, err := tr.Get(context.Background(), domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "PROJ-1"}); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("wrong provider err = %v", err)
	}
	for _, native := range []string{"--help", "PROJ", "PROJ-1; rm"} {
		if _, err := tr.Get(context.Background(), domain.TrackerID{Provider: domain.TrackerProviderJira, Native: native}); !errors.Is(err, ErrBadID) {
			t.Fatalf("Get(%q) err = %v, want ErrBadID", native, err)
		}
	}
}

func TestListBuildsQueryAndHydrates(t *testing.T) {
	cli := &fakeCLI{outputs: map[string]string{
		"issue list":              `[{"key": "PROJ-7", "fields": {}}]`,
		"issue view PROJ-7 --raw": cloudIssue,
	}}
	tr := NewWithRunner(cli.run)
	got, err := tr.List(context.Background(),
		domain.TrackerRepo{Provider: domain.TrackerProviderJira, Native: "PROJ"},
		domain.ListFilter{State: domain.ListOpen, Labels: []string{`a"b`}, Assignee: "peter@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID.Native != "PROJ-7" || got[0].State != domain.IssueOpen {
		t.Fatalf("List = %#v", got)
	}
	wantArgs := []string{"issue", "list", "--raw", "--project", "PROJ", "--paginate", "0:100",
		"--assignee", "peter@example.com", "--jql", `statusCategory != Done AND labels = "a\"b"`}
	if !reflect.DeepEqual(cli.calls[0], wantArgs) {
		t.Fatalf("list args:\n got %q\nwant %q", cli.calls[0], wantArgs)
	}
}

func TestListAssigneeWildcardsAndEmptyResult(t *testing.T) {
	cli := &fakeCLI{errs: map[string]error{"issue list": errors.New(`jira issue: exit status 1: ✗ No result found for given query in project "PROJ"`)}}
	tr := NewWithRunner(cli.run)
	got, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderJira, Native: "PROJ"}, domain.ListFilter{Assignee: "*", Limit: 5})
	if err != nil || got != nil {
		t.Fatalf("List = %#v, %v; want empty", got, err)
	}
	wantArgs := []string{"issue", "list", "--raw", "--project", "PROJ", "--paginate", "0:5", "--jql", "assignee is not EMPTY"}
	if !reflect.DeepEqual(cli.calls[0], wantArgs) {
		t.Fatalf("list args:\n got %q\nwant %q", cli.calls[0], wantArgs)
	}
	if _, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderJira, Native: "-x"}, domain.ListFilter{}); !errors.Is(err, ErrBadID) {
		t.Fatalf("bad project err = %v", err)
	}
}
