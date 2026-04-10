package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const apiBase = "https://api.github.com"

// Client is a GitHub REST API client.
type Client struct {
	token      string
	mode       string // "enterprise" or "organization"
	enterprise string
	org        string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// NewClient returns a new GitHub API client.
// mode must be "enterprise" or "organization".
// enterprise is the enterprise slug (required for enterprise mode).
// org is the organization name (required for organization mode).
func NewClient(token, mode, enterprise, org string) *Client {
	return &Client{
		token:      token,
		mode:       mode,
		enterprise: enterprise,
		org:        org,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		// 10 req/s — well within GitHub's 5,000/hour primary limit and
		// safe against secondary rate limits for sequential requests.
		limiter: rate.NewLimiter(rate.Every(100*time.Millisecond), 1),
	}
}

// Member represents a GitHub user in the context of org or enterprise membership.
type Member struct {
	Login      string
	Email      string // may be empty if the user has a private GitHub email
	Name       string
	Role       string // "member", "owner" (enterprise) or "member", "admin" (org)
	MemberType string // "member", "outside_collaborator", or "pending_invitation"
	CreatedAt  string // YYYY-MM-DD from GitHub account creation, empty for pending-by-email
}

// --- raw API response types ---

type apiUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "User" or "Bot"
}

type apiUserDetail struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"` // RFC3339
}

type apiInvitation struct {
	Login string `json:"login"` // empty if invited by email only
	Email string `json:"email"` // empty if invited by GitHub login
}

// --- Public methods ---

// ValidateConnection probes the configured GitHub API endpoint and returns a
// clear error if the token is invalid, lacks required scope, or the
// enterprise/org slug is not found.
func (c *Client) ValidateConnection(ctx context.Context) error {
	var path string
	switch c.mode {
	case "enterprise":
		path = fmt.Sprintf("/enterprises/%s/members?per_page=1", c.enterprise)
	case "organization":
		path = fmt.Sprintf("/orgs/%s/members?per_page=1", c.org)
	default:
		return fmt.Errorf("unknown mode %q", c.mode)
	}

	req, err := c.newRequest(ctx, http.MethodGet, apiBase+path)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &apiErr)

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("github: invalid or missing token (401) — check github.token or GITHUB_TOKEN")
	case http.StatusForbidden:
		return fmt.Errorf("github: access forbidden (403): %s — ensure the PAT has %s scope and the authenticated user is an enterprise owner",
			apiErr.Message, c.requiredScope())
	case http.StatusNotFound:
		if c.mode == "enterprise" {
			return fmt.Errorf(
				"github: enterprise %q returned 404 — most likely cause: PAT has read:enterprise but needs admin:enterprise (the members API requires full enterprise admin scope, not just profile read); also verify the PAT user is an enterprise owner at github.com/enterprises/%s/people",
				c.enterprise, c.enterprise)
		}
		return fmt.Errorf("github: organization %q not found (404) — check the org name", c.org)
	default:
		return fmt.Errorf("github: unexpected status %d: %s", resp.StatusCode, apiErr.Message)
	}
}

// ListMembers returns all active members of the configured enterprise or organization,
// including their role. Bot accounts are excluded. Email and Name fields are not
// populated here — call EnrichMembers to fetch them.
func (c *Client) ListMembers(ctx context.Context) ([]Member, error) {
	switch c.mode {
	case "enterprise":
		return c.listByRoles(ctx,
			fmt.Sprintf("/enterprises/%s/members", c.enterprise),
			"member", "owner")
	case "organization":
		return c.listByRoles(ctx,
			fmt.Sprintf("/orgs/%s/members", c.org),
			"member", "admin")
	default:
		return nil, fmt.Errorf("unknown mode %q", c.mode)
	}
}

// ListOutsideCollaborators returns outside collaborators for the configured
// organization. Only valid in organization mode. Bot accounts are excluded.
func (c *Client) ListOutsideCollaborators(ctx context.Context) ([]Member, error) {
	if c.mode != "organization" {
		return nil, fmt.Errorf("outside collaborators are only available in organization mode")
	}
	users, err := c.fetchPagedUsers(ctx,
		fmt.Sprintf("/orgs/%s/outside_collaborators", c.org))
	if err != nil {
		return nil, err
	}
	members := make([]Member, len(users))
	for i, u := range users {
		members[i] = Member{Login: u.Login, MemberType: "outside_collaborator"}
	}
	return members, nil
}

// ListPendingInvitations returns pending org membership invitations for the
// configured organization. Only valid in organization mode.
// Members invited by email (no GitHub account yet) will have an empty Login
// and their email pre-populated; members invited by login will need EnrichMembers
// to resolve their email.
func (c *Client) ListPendingInvitations(ctx context.Context) ([]Member, error) {
	if c.mode != "organization" {
		return nil, fmt.Errorf("pending invitations are only available in organization mode")
	}

	var all []Member
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/orgs/%s/invitations?per_page=100&page=%d",
			apiBase, c.org, page)
		var items []apiInvitation
		if err := c.get(ctx, url, &items); err != nil {
			return nil, fmt.Errorf("listing pending invitations (page %d): %w", page, err)
		}
		for _, inv := range items {
			all = append(all, Member{
				Login:      inv.Login,
				Email:      strings.ToLower(inv.Email),
				MemberType: "pending_invitation",
			})
		}
		if len(items) < 100 {
			break
		}
	}
	return all, nil
}

// GetUserDetail fetches public profile information for a GitHub user.
// Returns name, email (empty if private), and account creation date (YYYY-MM-DD).
func (c *Client) GetUserDetail(ctx context.Context, login string) (name, email, createdAt string, err error) {
	var u apiUserDetail
	if err := c.get(ctx, fmt.Sprintf("%s/users/%s", apiBase, login), &u); err != nil {
		return "", "", "", fmt.Errorf("fetching user detail for %s: %w", login, err)
	}
	if len(u.CreatedAt) >= 10 {
		createdAt = u.CreatedAt[:10]
	}
	return u.Name, strings.ToLower(u.Email), createdAt, nil
}

// --- internal helpers ---

// listByRoles fetches members matching each role in roles and merges the results.
// The GitHub API does not return role in the response body — role is inferred from
// the query parameter used to retrieve each batch.
func (c *Client) listByRoles(ctx context.Context, basePath string, roles ...string) ([]Member, error) {
	var all []Member
	for _, role := range roles {
		path := basePath + "?role=" + role
		users, err := c.fetchPagedUsers(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("listing %s (role=%s): %w", basePath, role, err)
		}
		for _, u := range users {
			all = append(all, Member{
				Login:      u.Login,
				Role:       role,
				MemberType: "member",
			})
		}
	}
	return all, nil
}

// fetchPagedUsers pages through a GitHub endpoint that returns []apiUser,
// filtering out Bot accounts. basePath must not include pagination params.
func (c *Client) fetchPagedUsers(ctx context.Context, basePath string) ([]apiUser, error) {
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	var all []apiUser
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s%s%sper_page=100&page=%d", apiBase, basePath, sep, page)
		var items []apiUser
		if err := c.get(ctx, url, &items); err != nil {
			return nil, err
		}
		for _, u := range items {
			if u.Type == "Bot" {
				continue
			}
			all = append(all, u)
		}
		if len(items) < 100 {
			break
		}
	}
	return all, nil
}

func (c *Client) requiredScope() string {
	if c.mode == "enterprise" {
		return "admin:enterprise"
	}
	return "read:org"
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodGet, url)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}

	// Handle rate limiting
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 403 {
		retryAfter := resp.Header.Get("Retry-After")
		if retryAfter != "" {
			return fmt.Errorf("github GET %s: rate limited — retry after %s seconds", url, retryAfter)
		}
	}

	body, _ := io.ReadAll(resp.Body)
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &apiErr)
	if apiErr.Message != "" {
		return fmt.Errorf("github GET %s: status %d: %s", url, resp.StatusCode, apiErr.Message)
	}
	return fmt.Errorf("github GET %s: status %d", url, resp.StatusCode)
}

func (c *Client) newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}
