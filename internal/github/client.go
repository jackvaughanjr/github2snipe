package github

import (
	"bytes"
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
	// organizations is used in enterprise mode when the enterprise members API
	// is not available (traditional GHEC without EMU). When set, members are
	// enumerated from each org and deduplicated by login.
	organizations []string
	httpClient    *http.Client
	limiter       *rate.Limiter
}

// NewClient returns a new GitHub API client.
// mode must be "enterprise" or "organization".
// enterprise is the enterprise slug (enterprise mode).
// org is the single org name (organization mode).
// organizations is an optional list of org names used in enterprise mode to
// enumerate members via org APIs instead of the enterprise members API.
func NewClient(token, mode, enterprise, org string, organizations []string) *Client {
	return &Client{
		token:         token,
		mode:          mode,
		enterprise:    enterprise,
		org:           org,
		organizations: organizations,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
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

// GraphQL response types for SAML/SCIM identity queries.
type gqlSAMLResponse struct {
	Data struct {
		Organization *struct {
			SAMLIdentityProvider *struct {
				ExternalIdentities struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						SAMLIdentity *struct {
							NameID string `json:"nameId"`
						} `json:"samlIdentity"`
						SCIMIdentity *struct {
							Username string `json:"username"`
						} `json:"scimIdentity"`
						User *struct {
							Login string `json:"login"`
						} `json:"user"`
					} `json:"nodes"`
				} `json:"externalIdentities"`
			} `json:"samlIdentityProvider"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

const samlIdentitiesQuery = `
query SAMLIdentities($org: String!, $after: String) {
  organization(login: $org) {
    samlIdentityProvider {
      externalIdentities(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          samlIdentity { nameId }
          scimIdentity { username }
          user { login }
        }
      }
    }
  }
}`

// GraphQL response types for verified domain email queries.
type gqlVerifiedEmailsResponse struct {
	Data struct {
		Organization *struct {
			MembersWithRole struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					Login  string   `json:"login"`
					Emails []string `json:"organizationVerifiedDomainEmails"`
				} `json:"nodes"`
			} `json:"membersWithRole"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

const verifiedDomainEmailsQuery = `
query OrgMemberVerifiedEmails($org: String!, $after: String) {
  organization(login: $org) {
    membersWithRole(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        login
        organizationVerifiedDomainEmails(login: $org)
      }
    }
  }
}`

// --- Public methods ---

// ValidateConnection probes the configured GitHub API endpoint and returns a
// clear error if the token is invalid, lacks required scope, or the target
// is not accessible.
func (c *Client) ValidateConnection(ctx context.Context) error {
	var path string
	switch c.mode {
	case "enterprise":
		if len(c.organizations) > 0 {
			// Multi-org enterprise mode: probe the first configured org.
			path = fmt.Sprintf("/orgs/%s/members?per_page=1", c.organizations[0])
		} else {
			// EMU enterprise mode: probe the enterprise members endpoint.
			path = fmt.Sprintf("/enterprises/%s/members?per_page=1", c.enterprise)
		}
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
		if strings.Contains(apiErr.Message, "SAML") || strings.Contains(apiErr.Message, "single sign-on") {
			target := c.org
			if c.mode == "enterprise" && len(c.organizations) > 0 {
				target = c.organizations[0]
			}
			return fmt.Errorf(
				"github: SAML SSO enforcement (403) — the PAT must be authorized for the %q organization: "+
					"go to github.com/settings/tokens, click your token, then click \"Configure SSO\" → \"Authorize\" next to the org",
				target)
		}
		return fmt.Errorf("github: access forbidden (403): %s — ensure the PAT has %s scope",
			apiErr.Message, c.requiredScope())
	case http.StatusNotFound:
		if c.mode == "enterprise" && len(c.organizations) == 0 {
			return fmt.Errorf(
				"github: enterprise members API returned 404 for %q — "+
					"this API only works for Enterprise Managed Users (EMU) tenants; "+
					"traditional GitHub Enterprise Cloud accounts must enumerate via org APIs instead — "+
					"add your org name(s) to github.organizations in settings.yaml",
				c.enterprise)
		}
		target := c.org
		if c.mode == "enterprise" {
			target = c.organizations[0]
		}
		return fmt.Errorf("github: organization %q not found (404) — check the org name", target)
	default:
		return fmt.Errorf("github: unexpected status %d: %s", resp.StatusCode, apiErr.Message)
	}
}

// ListMembers returns all active members of the configured enterprise or organization,
// including their role. Bot accounts are excluded. Email and Name fields are not
// populated here — the syncer calls GetUserDetail to enrich them.
//
// Enterprise mode behaviour:
//   - If github.organizations is set: enumerates members from each org and
//     deduplicates by login. Works for traditional GHEC accounts.
//   - If github.organizations is empty: calls the enterprise members API directly.
//     Only works for EMU (Enterprise Managed Users) tenants.
func (c *Client) ListMembers(ctx context.Context) ([]Member, error) {
	switch c.mode {
	case "enterprise":
		if len(c.organizations) > 0 {
			return c.listMembersFromOrgs(ctx, c.organizations)
		}
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

// ListOutsideCollaborators returns outside collaborators. In organization mode
// it queries the single configured org. In enterprise mode with github.organizations
// set, it queries each configured org and deduplicates by login.
func (c *Client) ListOutsideCollaborators(ctx context.Context) ([]Member, error) {
	orgs := c.activeOrgs()
	if len(orgs) == 0 {
		return nil, fmt.Errorf("outside collaborators require organization mode or github.organizations set in enterprise mode")
	}

	seen := make(map[string]struct{})
	var all []Member
	for _, org := range orgs {
		users, err := c.fetchPagedUsers(ctx, fmt.Sprintf("/orgs/%s/outside_collaborators", org))
		if err != nil {
			return nil, fmt.Errorf("listing outside collaborators for org %s: %w", org, err)
		}
		for _, u := range users {
			if _, ok := seen[u.Login]; !ok {
				seen[u.Login] = struct{}{}
				all = append(all, Member{Login: u.Login, MemberType: "outside_collaborator"})
			}
		}
	}
	return all, nil
}

// ListPendingInvitations returns pending org membership invitations. In
// organization mode it queries the single configured org. In enterprise mode
// with github.organizations set, it queries each configured org and deduplicates.
func (c *Client) ListPendingInvitations(ctx context.Context) ([]Member, error) {
	orgs := c.activeOrgs()
	if len(orgs) == 0 {
		return nil, fmt.Errorf("pending invitations require organization mode or github.organizations set in enterprise mode")
	}

	seen := make(map[string]struct{})
	var all []Member
	for _, org := range orgs {
		for page := 1; ; page++ {
			url := fmt.Sprintf("%s/orgs/%s/invitations?per_page=100&page=%d", apiBase, org, page)
			var items []apiInvitation
			if err := c.get(ctx, url, &items); err != nil {
				return nil, fmt.Errorf("listing pending invitations for org %s (page %d): %w", org, page, err)
			}
			for _, inv := range items {
				key := inv.Login
				if key == "" {
					key = strings.ToLower(inv.Email)
				}
				if key == "" {
					continue
				}
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					all = append(all, Member{
						Login:      inv.Login,
						Email:      strings.ToLower(inv.Email),
						MemberType: "pending_invitation",
					})
				}
			}
			if len(items) < 100 {
				break
			}
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

// GetSAMLIdentities returns a login→email map built from SAML SSO identity
// records for the configured org(s). The returned email is the SAML NameID
// asserted by the identity provider — for most corporate setups this is the
// company-managed email address.
//
// Returns an empty map (not an error) when:
//   - the org has no SAML identity provider configured
//   - the PAT owner lacks org admin permissions to list all identities
//   - running in EMU enterprise mode (no orgs configured)
//
// Use this as a fallback to resolve email addresses for GitHub users who have
// set their GitHub profile email to private.
func (c *Client) GetSAMLIdentities(ctx context.Context) (map[string]string, error) {
	orgs := c.activeOrgs()
	if len(orgs) == 0 {
		return map[string]string{}, nil
	}
	result := make(map[string]string)
	for _, org := range orgs {
		m, err := c.samlIdentitiesForOrg(ctx, org)
		if err != nil {
			return nil, fmt.Errorf("fetching SAML identities for org %s: %w", org, err)
		}
		for login, email := range m {
			if _, exists := result[login]; !exists {
				result[login] = email
			}
		}
	}
	return result, nil
}

// GetVerifiedDomainEmails returns a login→email map built from the
// organizationVerifiedDomainEmails field on each org member. This returns
// member emails that match the org's verified domain(s) — visible to org admins
// even when the user's GitHub profile email is set to private.
//
// Used as a fallback after SAML/SCIM identity lookup for members who have not
// authenticated via SSO but whose company email is verifiably associated with
// their GitHub account.
//
// Returns an empty map (not an error) when:
//   - the org has no verified domains configured
//   - the PAT owner lacks org admin permissions
//   - running in EMU enterprise mode (no orgs configured)
func (c *Client) GetVerifiedDomainEmails(ctx context.Context) (map[string]string, error) {
	orgs := c.activeOrgs()
	if len(orgs) == 0 {
		return map[string]string{}, nil
	}
	result := make(map[string]string)
	for _, org := range orgs {
		m, err := c.verifiedDomainEmailsForOrg(ctx, org)
		if err != nil {
			return nil, fmt.Errorf("fetching verified domain emails for org %s: %w", org, err)
		}
		for login, email := range m {
			if _, exists := result[login]; !exists {
				result[login] = email
			}
		}
	}
	return result, nil
}

// --- internal helpers ---

// activeOrgs returns the org(s) to use for org-level API calls.
// In organization mode it returns the single configured org.
// In enterprise mode it returns the github.organizations list.
func (c *Client) activeOrgs() []string {
	if c.mode == "organization" {
		return []string{c.org}
	}
	return c.organizations
}

// listMembersFromOrgs enumerates members (members + admins) from each org
// in the list and deduplicates by login. Used in enterprise mode when the
// enterprise members API is unavailable (traditional GHEC without EMU).
func (c *Client) listMembersFromOrgs(ctx context.Context, orgs []string) ([]Member, error) {
	seen := make(map[string]struct{})
	var all []Member
	for _, org := range orgs {
		members, err := c.listByRoles(ctx,
			fmt.Sprintf("/orgs/%s/members", org),
			"member", "admin")
		if err != nil {
			return nil, fmt.Errorf("listing members from org %s: %w", org, err)
		}
		for _, m := range members {
			if _, ok := seen[m.Login]; !ok {
				seen[m.Login] = struct{}{}
				all = append(all, m)
			}
		}
	}
	return all, nil
}

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
	if c.mode == "enterprise" && len(c.organizations) == 0 {
		// EMU path: enterprise members API.
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

// samlIdentitiesForOrg paginates through the SAML external identities for one
// org and returns a login→email map. Returns an empty map without error when
// the org has no SAML identity provider configured or the PAT owner does not
// have org admin access.
func (c *Client) samlIdentitiesForOrg(ctx context.Context, org string) (map[string]string, error) {
	result := make(map[string]string)
	var cursor any // nil on first page; string cursor on subsequent pages
	for {
		vars := map[string]any{"org": org, "after": cursor}
		var resp gqlSAMLResponse
		if err := c.graphqlPost(ctx, samlIdentitiesQuery, vars, &resp); err != nil {
			// A non-200 HTTP status (e.g. 403 Forbidden) means the PAT lacks
			// org admin permissions. Treat as empty — not a fatal sync error.
			return map[string]string{}, nil
		}
		if len(resp.Errors) > 0 {
			// GraphQL-level errors (e.g. "Must be an organization owner") are
			// also treated as empty rather than fatal.
			return map[string]string{}, nil
		}
		if resp.Data.Organization == nil || resp.Data.Organization.SAMLIdentityProvider == nil {
			return result, nil // org exists but has no SAML provider configured
		}
		identities := resp.Data.Organization.SAMLIdentityProvider.ExternalIdentities
		for _, node := range identities.Nodes {
			if node.User == nil {
				continue
			}
			login := strings.ToLower(node.User.Login)
			// Prefer SAML NameID (asserted at login time); fall back to SCIM
			// username (provisioned by the IdP via SCIM push, present even before
			// the user has authenticated interactively via SSO).
			var email string
			if node.SAMLIdentity != nil && node.SAMLIdentity.NameID != "" {
				email = strings.ToLower(node.SAMLIdentity.NameID)
			} else if node.SCIMIdentity != nil && node.SCIMIdentity.Username != "" {
				email = strings.ToLower(node.SCIMIdentity.Username)
			}
			if login != "" && email != "" {
				result[login] = email
			}
		}
		if !identities.PageInfo.HasNextPage {
			break
		}
		cursor = identities.PageInfo.EndCursor
	}
	return result, nil
}

// verifiedDomainEmailsForOrg paginates through org members and returns a
// login→email map of first verified-domain email per member. Returns an empty
// map without error on permission failures or when no verified domains exist.
func (c *Client) verifiedDomainEmailsForOrg(ctx context.Context, org string) (map[string]string, error) {
	result := make(map[string]string)
	var cursor any
	for {
		vars := map[string]any{"org": org, "after": cursor}
		var resp gqlVerifiedEmailsResponse
		if err := c.graphqlPost(ctx, verifiedDomainEmailsQuery, vars, &resp); err != nil {
			return map[string]string{}, nil
		}
		if len(resp.Errors) > 0 {
			return map[string]string{}, nil
		}
		if resp.Data.Organization == nil {
			return result, nil
		}
		members := resp.Data.Organization.MembersWithRole
		for _, node := range members.Nodes {
			if node.Login == "" || len(node.Emails) == 0 {
				continue
			}
			login := strings.ToLower(node.Login)
			email := strings.ToLower(node.Emails[0])
			if login != "" && email != "" {
				result[login] = email
			}
		}
		if !members.PageInfo.HasNextPage {
			break
		}
		cursor = members.PageInfo.EndCursor
	}
	return result, nil
}

// graphqlPost sends a GraphQL POST request to the GitHub API and decodes the
// response JSON into out. Applies rate limiting before sending.
func (c *Client) graphqlPost(ctx context.Context, query string, variables map[string]any, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github GraphQL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github GraphQL: status %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
