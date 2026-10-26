package gitlab

import (
	"context"
	"reflect"
	"testing"
)

func TestParseGLabHosts(t *testing.T) {
	out := `gitlab.com
  ✓ Logged in to gitlab.com as alice (/home/alice/.config/glab-cli/config.yml)
  ✓ Token: **************************
Gitlab.Example.com
  ✓ Logged in to Gitlab.Example.com as alice (keyring)
broken.example
  x broken.example: API call failed: 401 Unauthorized
`
	got := parseGLabHosts(out)
	want := []string{"gitlab.com", "gitlab.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseGLabHosts = %v, want %v", got, want)
	}
	if hosts := parseGLabHosts("glab: command not found"); len(hosts) != 0 {
		t.Fatalf("parseGLabHosts(no glab) = %v, want none", hosts)
	}
}

// TestDiscoveredHostTrustedWithItsOwnToken verifies a host glab is signed in
// to parses as GitLab and its client uses the host-scoped token, while an
// unknown host stays rejected.
func TestDiscoveredHostTrustedWithItsOwnToken(t *testing.T) {
	discoveries := 0
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("gitlab-com-token"),
		SkipTokenPreflight: true,
		DiscoverHosts: func(context.Context) []string {
			discoveries++
			return []string{"Git.Corp.Example"}
		},
		HostToken: func(host string) TokenSource { return StaticTokenSource("token-for-" + host) },
	})
	if err != nil {
		t.Fatal(err)
	}

	repo, ok := p.ParseRepository("git@git.corp.example:team/sub/app.git")
	if !ok || repo.Provider != "gitlab" || repo.Host != "git.corp.example" || repo.Repo != "team/sub/app" {
		t.Fatalf("ParseRepository(discovered host) = %+v, %v", repo, ok)
	}
	if _, ok := p.ParseRepository("https://unknown.example/team/app.git"); ok {
		t.Fatal("ParseRepository accepted a host glab is not signed in to")
	}
	if discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1 (cached)", discoveries)
	}

	client := p.clientForHost("git.corp.example")
	if client == nil {
		t.Fatal("clientForHost(discovered host) = nil")
	}
	tok, err := client.tokens.Token(context.Background())
	if err != nil || tok != "token-for-git.corp.example" {
		t.Fatalf("discovered host token = %q, %v; want host-scoped token", tok, err)
	}

	if ok, err := p.SCMCredentialsAvailable(context.Background()); !ok || err != nil {
		t.Fatalf("SCMCredentialsAvailable = %v, %v", ok, err)
	}
}
