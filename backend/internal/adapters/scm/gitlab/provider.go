package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ProviderOptions configures the GitLab SCM provider.
type ProviderOptions struct {
	Client             *Client
	HTTPClient         *http.Client
	Token              TokenSource
	SkipTokenPreflight bool
	RESTBase           string
	UserAgent          string
	Logger             *slog.Logger

	// AllowedHosts is the list of self-managed GitLab hosts that the provider
	// is permitted to talk to. gitlab.com is always allowed (hardcoded) and
	// does not need to appear here. A host not in this list and not gitlab.com
	// is rejected before any credential is attached — preventing a remote like
	// gitlab.attacker.example from receiving the configured bearer token.
	// Each entry may include a port (e.g. "gitlab.internal:8443").
	AllowedHosts []string

	// HostTokens maps a self-managed host to a token override. Hosts in
	// AllowedHosts without an explicit entry fall back to the default Token
	// (typically AO_GITLAB_TOKEN / GITLAB_TOKEN / glab). The per-host
	// selection ensures the gitlab.com token is not attached to a self-managed
	// host and vice versa.
	HostTokens map[string]TokenSource

	// DiscoverHosts lists additional self-managed hosts the user has signed in
	// to (production: DiscoverGLabHosts). They are trusted like AllowedHosts.
	// The result is cached for discoveredHostsTTL.
	DiscoverHosts func(ctx context.Context) []string

	// HostToken builds the token source for a trusted self-managed host that
	// has no HostTokens entry. Nil falls back to the default Token.
	HostToken func(host string) TokenSource
}

const discoveredHostsTTL = 5 * time.Minute

// Provider implements the provider-neutral scm.Provider interface for GitLab.
// It supports multiple GitLab hosts (gitlab.com + self-managed) by maintaining
// a per-host Client map, so a self-managed host's REST base resolves from its
// hostname rather than being hardcoded to gitlab.com.
//
// Hosts are restricted to gitlab.com plus an explicit allowlist
// (ProviderOptions.AllowedHosts). A host that is neither gitlab.com nor in the
// allowlist is rejected before any credential is attached — no HTTP request is
// made to non-allowlisted hosts.
type Provider struct {
	client *Client // default client (gitlab.com)
	logger *slog.Logger

	// allowedHosts is the set of self-managed hosts the provider may talk to
	// (gitlab.com is always allowed and not stored here). Each entry may include
	// a port (e.g. "gitlab.internal:8443").
	allowedHosts map[string]bool

	// hostTokens maps a self-managed host to its TokenSource. Hosts in
	// allowedHosts without an entry fall back to the default token (opts.Token).
	hostTokens map[string]TokenSource
	hostToken  func(host string) TokenSource

	discoverHosts   func(ctx context.Context) []string
	discoveredMu    sync.Mutex
	discovered      map[string]bool
	discoveredUntil time.Time

	// Per-host clients for self-managed GitLab instances. Lazily populated.
	hostClients   map[string]*Client
	hostClientCfg ClientOptions
	hostMu        sync.Mutex

	// cache is the provider-level in-memory TTL cache shared across three
	// domains: MR detail (60s), approvals (60s), and fork-project path
	// resolution (5min). It is transparent to callers — the scm.Provider
	// interface is unchanged. Populated and consulted by FetchPullRequests,
	// FetchReviewThreads, ListPRsByRepo, and the fork-resolution path.
	cache *cache

	// hostIdentities caches the authenticated account per-host so each host
	// is resolved at most once for the provider's lifetime. The key is the
	// normalized host string (empty string = gitlab.com).
	hostIdentities map[string]ports.SCMIdentity
	identityMu     sync.Mutex
}

// NewProvider creates a GitLab SCM provider. If SkipTokenPreflight is false
// and a token source is supplied, the token is resolved immediately to fail
// fast on misconfiguration.
func NewProvider(opts ProviderOptions) (*Provider, error) {
	if opts.Client == nil && opts.Token != nil && !opts.SkipTokenPreflight {
		_, err := opts.Token.Token(context.Background())
		if err != nil {
			return nil, err
		}
	}
	c := opts.Client
	if c == nil {
		c = NewClient(ClientOptions{
			HTTPClient: opts.HTTPClient,
			Token:      opts.Token,
			RESTBase:   opts.RESTBase,
			UserAgent:  opts.UserAgent,
		})
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	allowed := make(map[string]bool, len(opts.AllowedHosts))
	for _, h := range opts.AllowedHosts {
		h = NormalizeHost(h)
		if h != "" {
			allowed[h] = true
		}
	}

	hostTokens := make(map[string]TokenSource, len(opts.HostTokens))
	for h, src := range opts.HostTokens {
		h = NormalizeHost(h)
		if h != "" && src != nil {
			hostTokens[h] = src
		}
	}

	return &Provider{
		client:        c,
		logger:        logger,
		allowedHosts:  allowed,
		hostTokens:    hostTokens,
		hostToken:     opts.HostToken,
		discoverHosts: opts.DiscoverHosts,
		hostClientCfg: ClientOptions{
			HTTPClient: opts.HTTPClient,
			Token:      opts.Token,
			UserAgent:  opts.UserAgent,
			RESTBase:   opts.RESTBase,
		},
		hostClients: map[string]*Client{},
		cache:       newCache(),
	}, nil
}

// isHostAllowed reports whether host is a GitLab host the provider is permitted
// to talk to. gitlab.com is always allowed; self-managed hosts must be in the
// configured allowlist.
func (p *Provider) isHostAllowed(host string) bool {
	host = NormalizeHost(host)
	if host == "" {
		return false
	}
	if IsGitLabDotCom(host) {
		return true
	}
	return p.isSelfManagedTrusted(host)
}

// isSelfManagedTrusted reports whether a normalized self-managed host is
// allowlisted or one the user signed in to through glab.
func (p *Provider) isSelfManagedTrusted(host string) bool {
	if p.allowedHosts[host] {
		return true
	}
	if p.discoverHosts == nil {
		return false
	}
	p.discoveredMu.Lock()
	defer p.discoveredMu.Unlock()
	if time.Now().After(p.discoveredUntil) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		hosts := p.discoverHosts(ctx)
		cancel()
		p.discovered = make(map[string]bool, len(hosts))
		for _, h := range hosts {
			p.discovered[NormalizeHost(h)] = true
		}
		p.discoveredUntil = time.Now().Add(discoveredHostsTTL)
	}
	return p.discovered[host]
}

// tokenForHost picks the token for a trusted self-managed host: an explicit
// HostTokens entry, then the HostToken builder, then the default token.
func (p *Provider) tokenForHost(host string) TokenSource {
	if src, ok := p.hostTokens[host]; ok {
		return src
	}
	if p.hostToken != nil {
		return p.hostToken(host)
	}
	return p.hostClientCfg.Token
}

// clientForHost returns the client whose REST base matches the given host.
// The default client (gitlab.com) is used for gitlab.com; self-managed hosts
// get a lazily-created client with RESTBase derived as https://<host>/api/v4.
// A host that is neither gitlab.com nor in the allowlist is rejected — nil is
// returned so callers can fail closed before attaching any credential.
//
// The REST base preserves the port from the host (e.g.
// "gitlab.internal:8443" → "https://gitlab.internal:8443/api/v4"). The scheme
// is always https for self-managed hosts; a self-managed host reachable over
// plain http is a deployment choice outside this provider's scope and is not
// supported (matching the reviewer's "reject HTTPS-to-HTTP downgrades" rule
// from Item 6).
func (p *Provider) clientForHost(host string) *Client {
	h := NormalizeHost(host)
	// Empty host (e.g. test repos without Host set) falls back to the default
	// client so tests using a test-server RESTBase keep working. gitlab.com and
	// www.gitlab.com also use the default client.
	if IsGitLabDotCom(h) {
		return p.client
	}
	// Reject untrusted hosts before any credential is attached.
	if !p.isSelfManagedTrusted(h) {
		return nil
	}
	// If the default client's RESTBase already matches this host (e.g. a test
	// server whose host happens to be in the allowlist), reuse it.
	if p.client != nil && hostMatchesRESTBase(p.client.restBase, h) {
		return p.client
	}

	p.hostMu.Lock()
	defer p.hostMu.Unlock()
	if c, ok := p.hostClients[h]; ok {
		return c
	}

	cfg := p.hostClientCfg
	// Derive the REST base from the host, preserving any port (e.g.
	// "gitlab.internal:8443" → "https://gitlab.internal:8443/api/v4").
	cfg.RESTBase = "https://" + h + "/api/v4"
	cfg.Token = p.tokenForHost(h)
	c := NewClient(cfg)
	p.hostClients[h] = c
	return c
}

// hostMatchesRESTBase reports whether host is the hostname of restBase.
func hostMatchesRESTBase(restBase, host string) bool {
	if restBase == "" {
		return false
	}
	// restBase is like "https://gitlab.com/api/v4" — extract the host.
	trimmed := strings.TrimPrefix(strings.TrimPrefix(restBase, "https://"), "http://")
	if idx := strings.IndexByte(trimmed, '/'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.EqualFold(trimmed, host)
}

// ErrHostNotAllowed is returned when a remote's host is neither gitlab.com nor
// in the configured allowlist. The provider rejects such hosts before
// attaching any credential.
var ErrHostNotAllowed = fmt.Errorf("gitlab scm: host not in allowlist")

// clientForRepoErr returns the client for a repo's host, or an error if the
// host is not allowed. This is the guarded variant of clientForHost for use in
// methods that need to return an error rather than panic on a nil client.
func (p *Provider) clientForRepoErr(repo ports.SCMRepo) (*Client, error) {
	c := p.clientForHost(repo.Host)
	if c == nil {
		return nil, fmt.Errorf("gitlab scm: host %q not in allowlist: %w", repo.Host, ErrHostNotAllowed)
	}
	return c, nil
}

// AuthenticatedIdentity returns the account associated with the active GitLab
// token. Successful results are cached for the provider's lifetime.
// GitLab has no typed Bot flag like GitHub's user.Type, so human/bot
// classification reuses the existing isBotAuthor heuristic (project/group
// bot username patterns, [bot] suffixes, known bot accounts).
func (p *Provider) AuthenticatedIdentity(ctx context.Context) (ports.SCMIdentity, error) {
	return p.AuthenticatedIdentityForHost(ctx, "")
}

// AuthenticatedIdentityForHost resolves the account associated with the
// GitLab token for the given host. For gitlab.com (or empty string) it uses
// the default client; for self-managed hosts it uses clientForHost to select
// the correct client and token. Successful results are cached per-host for
// the provider's lifetime — a second call for the same host does not hit the
// API.
//
// A host that is neither gitlab.com nor in the allowlist returns an error.
func (p *Provider) AuthenticatedIdentityForHost(ctx context.Context, host string) (ports.SCMIdentity, error) {
	key := NormalizeHost(host)
	p.identityMu.Lock()
	defer p.identityMu.Unlock()
	if p.hostIdentities == nil {
		p.hostIdentities = make(map[string]ports.SCMIdentity)
	}
	if ident, ok := p.hostIdentities[key]; ok {
		return ident, nil
	}

	client := p.clientForHost(host)
	if client == nil {
		return ports.SCMIdentity{}, fmt.Errorf("gitlab scm: host %q not in allowlist: %w", host, ErrHostNotAllowed)
	}

	resp, err := client.doGET(ctx, "/user", nil)
	if err != nil {
		return ports.SCMIdentity{}, err
	}

	var user struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(resp.Body, &user); err != nil {
		return ports.SCMIdentity{}, fmt.Errorf("gitlab scm: decode authenticated user: %w", err)
	}

	login := strings.TrimSpace(user.Username)
	ident := ports.SCMIdentity{
		Login: login,
		Human: !isBotAuthor(login),
	}
	p.hostIdentities[key] = ident
	return ident, nil
}

// SCMCredentialsAvailable reports whether usable GitLab credentials exist.
// It checks the default token source first, then falls back to per-host
// token sources (AO_GITLAB_HOST_TOKENS). If any token source (default or
// host-specific) is usable, it returns true. This ensures the observer is
// not disabled when a user configures only self-managed host tokens without
// a default token.
func (p *Provider) SCMCredentialsAvailable(ctx context.Context) (bool, error) {
	// Check the default token source first.
	if p.client != nil && p.client.tokens != nil {
		_, err := p.client.tokens.Token(ctx)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrNoToken) {
			return false, err
		}
	}

	// Default token is not available — check per-host tokens.
	for _, src := range p.hostTokens {
		if src == nil {
			continue
		}
		_, err := src.Token(ctx)
		if err == nil {
			return true, nil
		}
		// If a host token source returns a non-ErrNoToken error (e.g. glab
		// command failed), propagate it so the caller knows credentials exist
		// but are misconfigured.
		if !errors.Is(err, ErrNoToken) {
			return false, err
		}
	}

	// Finally, a self-managed host signed in through glab.
	if p.discoverHosts != nil && p.hostToken != nil {
		for _, host := range p.discoverHosts(ctx) {
			if _, err := p.hostToken(NormalizeHost(host)).Token(ctx); err == nil {
				return true, nil
			}
		}
	}

	return false, nil
}
