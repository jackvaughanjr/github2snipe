# CONTEXT.md — github2snipe

This file documents everything specific to the GitHub integration.
Cross-cutting conventions live in `CLAUDE.md`.

---

## Purpose

Syncs active GitHub Enterprise or GitHub Organization members into Snipe-IT as
license seat assignments. A single Snipe-IT license record is maintained for the
configured GitHub tenant. All member types (regular members, owners/admins, outside
collaborators, and pending invitations) can be included based on configuration.

The integration supports two modes:

- **enterprise**: Syncs all members of a GitHub Enterprise tenant via the Enterprise
  REST API. Recommended when licensing is tracked at the enterprise level.
- **organization**: Syncs members of a single GitHub Organization. Supports outside
  collaborators and pending org membership invitations.

Auth via a Personal Access Token (PAT).

---

## Auth method

**Personal Access Token (PAT)**

Create a PAT at GitHub → Settings → Developer Settings → Personal access tokens.

Required scopes and roles by mode:

| Mode                                  | Required PAT scope  | Required GitHub role               |
|---------------------------------------|---------------------|------------------------------------|
| `enterprise` (EMU)                    | `admin:enterprise`  | Enterprise Owner                   |
| `enterprise` + `github.organizations` | `read:org`          | Organization member (any role)     |
| `organization`                        | `read:org`          | Organization member (any role)     |

There are two enterprise mode paths with different requirements:

**EMU (Enterprise Managed Users):** When `github.organizations` is empty, the integration
calls the enterprise members API directly (`GET /enterprises/{slug}/members`). This only
works for EMU tenants. Requires `admin:enterprise` scope and the PAT owner must be an
Enterprise Owner. GitHub returns 404 (not 403) when the PAT lacks sufficient scope or
the user is not an enterprise owner — intentional security behaviour to avoid confirming
the existence of enterprise resources. Do not use `read:enterprise` — it only reads the
enterprise public profile and cannot list members.

**Traditional GHEC (non-EMU):** When `github.organizations` is set, the integration
enumerates members from each listed organization and deduplicates by login. This is the
correct path for traditional GitHub Enterprise Cloud accounts where users keep their
personal GitHub accounts. Requires `read:org` scope only.

Fine-grained PATs are supported. For EMU enterprise mode, the fine-grained token needs
the "Enterprise members: read" permission. For the organizations-based path, use the
standard `read:org` scope.

Set the token in `settings.yaml` as `github.token` or via the `GITHUB_TOKEN`
environment variable (this is the same env var used by GitHub Actions — convenient
if running this tool in a CI environment).

---

## API details

- **Base URL**: `https://api.github.com`
- **Required headers**: `Authorization: Bearer <token>`, `Accept: application/vnd.github+json`,
  `X-GitHub-Api-Version: 2022-11-28`
- **Rate limit**: 5,000 requests/hour per PAT. The client rate-limits itself to
  10 req/s to stay well within secondary rate limits.
- **Pagination**: Uses page-number query parameters (`?per_page=100&page=N`). Stops
  when a page returns fewer than `per_page` results.

### Enterprise mode endpoints

Enterprise mode supports two paths depending on your GitHub Enterprise Cloud type.

**EMU (Enterprise Managed Users) — `github.organizations` not set:**

| Purpose                     | Endpoint                                               |
|-----------------------------|--------------------------------------------------------|
| List members (role=member)  | `GET /enterprises/{enterprise}/members?role=member`    |
| List members (role=owner)   | `GET /enterprises/{enterprise}/members?role=owner`     |
| User profile (email, name)  | `GET /users/{login}`                                   |

Outside collaborators and pending invitations are **org-level** concepts and are not
available via the enterprise members API. These features are not supported in the
EMU path.

**Traditional GHEC — `github.organizations` set:**

| Purpose                     | Endpoint                                               |
|-----------------------------|--------------------------------------------------------|
| List members (role=member)  | `GET /orgs/{org}/members?role=member`                  |
| List members (role=admin)   | `GET /orgs/{org}/members?role=admin`                   |
| Outside collaborators       | `GET /orgs/{org}/outside_collaborators`                |
| Pending invitations         | `GET /orgs/{org}/invitations`                          |
| User profile (email, name)  | `GET /users/{login}`                                   |

All endpoints are called for each org in `github.organizations`. Results are
deduplicated by login across orgs.

### Organization mode endpoints

| Purpose                          | Endpoint                                         |
|----------------------------------|--------------------------------------------------|
| List members (role=member)       | `GET /orgs/{org}/members?role=member`            |
| List members (role=admin)        | `GET /orgs/{org}/members?role=admin`             |
| List outside collaborators       | `GET /orgs/{org}/outside_collaborators`          |
| List pending org invitations     | `GET /orgs/{org}/invitations`                    |
| User profile (email, name)       | `GET /users/{login}`                             |

### Email resolution (org mode and enterprise+organizations mode)

Member email addresses are resolved in priority order from three GraphQL sources,
all fetched once per sync before the per-user enrichment loop. In EMU enterprise
mode (no `github.organizations` set) all three are skipped — `activeOrgs()` returns
empty in that path.

| Priority | Source                         | Endpoint / query                                                                |
|----------|--------------------------------|---------------------------------------------------------------------------------|
| 1st      | SAML identity (NameID)         | `POST /graphql` — `organization.samlIdentityProvider.externalIdentities { samlIdentity { nameId } }` |
| 2nd      | SCIM identity (username)       | `POST /graphql` — same `externalIdentities` query — `scimIdentity { username }` |
| 3rd      | Org verified domain email      | `POST /graphql` — `organization.membersWithRole { organizationVerifiedDomainEmails(login: $org) }` |
| Fallback | Public GitHub profile email    | `GET /users/{login}` — `email` field (null when user has private email setting) |
| Skip     | None of the above              | Warning logged; user excluded from sync                                         |

**SAML/SCIM identity** (`externalIdentities`): The `samlIdentity.nameId` is the SAML
NameID asserted by the IdP at login time — typically the company-managed email for
orgs using Okta, Azure AD, or Google Workspace. `scimIdentity.username` is provisioned
by the IdP via SCIM push and is present even before the user has authenticated
interactively via SSO. SAML is preferred over SCIM; both take priority over all other
sources. When a SAML/SCIM identity exists for a user, it is used even if the user has
a different public profile email — the IdP-managed identity is the authoritative match
for Snipe-IT records.

**Org verified domain email** (`organizationVerifiedDomainEmails`): For members whose
org email is visible to org admins but who have no SAML/SCIM identity record (e.g.,
joined before SSO was enforced), GitHub exposes emails matching the org's verified
domain(s) via this field. Only emails matching a domain the org has verified are
returned. Requires the PAT owner to be an org admin.

**Graceful degradation**: all three GraphQL lookups return an empty map (not an error)
when the org has no SAML provider configured, the org has no verified domains, or the
PAT owner lacks org admin permissions. The sync continues in all cases; users who
cannot be resolved by any method are warned and skipped.

---

## Config schema

### settings.yaml keys

```yaml
github:
  # Connection mode: "enterprise" or "organization"
  mode: "enterprise"

  # GitHub Enterprise slug (required when mode: enterprise).
  # Found in enterprise URLs: https://github.com/enterprises/<slug>
  enterprise: "your-enterprise-slug"

  # GitHub Organization name (required when mode: organization).
  organization: "your-org-name"

  # Organizations to enumerate in enterprise mode (traditional GHEC accounts).
  # The enterprise members API only works for EMU tenants. Traditional GHEC accounts
  # (where users keep their personal GitHub accounts) must list members via org APIs.
  # Set this to the org slugs within your enterprise. Members are deduplicated across
  # all listed orgs. Outside collaborators and pending invitations also enumerate
  # across all listed orgs when enabled.
  # Leave empty only if your enterprise uses EMU (Enterprise Managed Users).
  # Has no effect when mode: organization.
  organizations:
    - "your-org-slug"

  # Personal Access Token (PAT).
  # Can be overridden with the GITHUB_TOKEN environment variable.
  token: ""

  # Optional prefix/suffix to wrap around the default license name.
  # Default: "GitHub Enterprise" (enterprise mode) or "GitHub" (organization mode).
  # Final name: {prefix}{default_name}{suffix}
  # Include separator characters in the values themselves.
  license_name_prefix: ""
  license_name_suffix: ""

  # Include outside collaborators in the sync.
  # Works in organization mode and in enterprise mode when github.organizations is set.
  # Outside collaborators consume GitHub license seats.
  include_outside_collaborators: false

  # Include pending org membership invitations.
  # Works in organization mode and in enterprise mode when github.organizations is set.
  # Pending invitations consume GitHub license seats.
  include_pending_invitations: false

snipe_it:
  url: "https://your-snipe-it-instance.example.com"
  api_key: ""

  # Override the computed license name. If set, prefix/suffix are not applied.
  # If unset, the name is: {prefix}{GitHub Enterprise|GitHub}{suffix}
  license_name: ""

  # Snipe-IT category ID for the GitHub license. Required.
  license_category_id: 0

  # Optional: manufacturer ID. If 0, "GitHub" is auto found/created.
  license_manufacturer_id: 0

  # Optional: supplier ID. If 0, no supplier is set.
  license_supplier_id: 0

sync:
  dry_run: false
  force: false
  create_users: false

slack:
  webhook_url: ""
```

### Environment variable overrides

| Env var         | Config key          |
|-----------------|---------------------|
| `GITHUB_TOKEN`  | `github.token`      |
| `SNIPE_URL`     | `snipe_it.url`      |
| `SNIPE_TOKEN`   | `snipe_it.api_key`  |
| `SLACK_WEBHOOK` | `slack.webhook_url` |

List-type and formatting config keys cannot be overridden via env vars; set them
in `settings.yaml`.

---

## License naming

The Snipe-IT license name is computed as:

```
{github.license_name_prefix}{default_name}{github.license_name_suffix}
```

Where `default_name` is:
- `"GitHub Enterprise"` when `mode: enterprise`
- `"GitHub"` when `mode: organization`

If `snipe_it.license_name` is set, it is used verbatim and prefix/suffix are ignored.

**Examples:**

| Mode         | Prefix         | Suffix          | Snipe-IT License Name               |
|--------------|----------------|-----------------|-------------------------------------|
| enterprise   | *(empty)*      | *(empty)*       | `GitHub Enterprise`                 |
| organization | *(empty)*      | *(empty)*       | `GitHub`                            |
| enterprise   | `"Acme - "`   | *(empty)*       | `Acme - GitHub Enterprise`          |
| organization | *(empty)*      | `" (acme.com)"` | `GitHub (acme.com)`                 |

---

## Seat notes format

Notes are written to Snipe-IT when a seat is first checked out. They are updated
on subsequent syncs only when the notes have changed or `--force` is used.

**Enterprise member (role=member):**
```
enterprise: acme-corp
role: member
github_login: agilemofo
```

**Enterprise member (role=owner):**
```
enterprise: acme-corp
role: owner
github_login: agilemofo
```

**Organization member (role=member):**
```
organization: acme-corp
role: member
github_login: agilemofo
```

**Organization admin (role=admin):**
```
organization: acme-corp
role: admin
github_login: agilemofo
```

**Outside collaborator:**
```
organization: acme-corp
type: outside_collaborator
github_login: agilemofo
```

**Pending org invitation (by login):**
```
organization: acme-corp
status: pending_invitation
github_login: agilemofo
```

**Pending org invitation (by email only, no GitHub account yet):**
```
organization: acme-corp
status: pending_invitation
```
*(No `github_login` line — the invitee has no GitHub account yet.)*

The enterprise slug or org name is included in the notes so that seats remain
identifiable if licenses are renamed or when multiple GitHub tenants are in use.
`github_login` maps the Snipe-IT seat back to the GitHub account regardless of
what email address the user has in their profile.

---

## Automatic user creation (`--create-users`)

By default, if a GitHub member has no Snipe-IT account, the sync warns, skips
them, and sends a Slack notification. With `--create-users` (or
`sync.create_users: true`), the sync creates the Snipe-IT account automatically
and then proceeds to check out the seat.

### Created user properties

| Field          | Value                                                                        |
|----------------|------------------------------------------------------------------------------|
| `first_name`   | Derived from the GitHub display name (text before first space), or email local-part |
| `last_name`    | Derived from the GitHub display name (text after first space), or empty       |
| `email`        | GitHub public email address                                                   |
| `username`     | Same as email                                                                 |
| `password`     | Cryptographically random 32-hex string (user cannot log in anyway)           |
| `activated`    | `false` — user cannot log into Snipe-IT                                      |
| `send_welcome` | `false` — no welcome email is sent                                            |
| `start_date`   | GitHub account creation date (`YYYY-MM-DD`)                                  |
| `notes`        | See below — varies by member type                                             |

### User creation notes by member type

| Member type           | Snipe-IT user `notes` field                                          |
|-----------------------|----------------------------------------------------------------------|
| Member / owner / admin| `Auto-created from GitHub via github2snipe`                          |
| Outside collaborator  | `Auto-created from GitHub via github2snipe (outside collaborator)`   |
| Pending invitation    | `Auto-created from GitHub via github2snipe (pending invitation)`     |

### Name derivation fallback

When a GitHub user's display name is not set, the first and last name are derived
from the email local-part by splitting on `.`:

- `jane.doe@example.com` → first: `jane`, last: `doe`
- `jdoe@example.com` → first: `jdoe`, last: *(empty)*

For pending invitations with no GitHub login (invited by email only, no account
yet), the name is derived from the invitation email using the same fallback.

### Pending invitations with no GitHub login

`GET /orgs/{org}/invitations` returns invitations where the invitee may not have
a GitHub account yet. In this case:
- `login` is null/empty — `GetUserDetail` cannot be called
- `email` is directly available from the invitation object
- Name must be derived from the email (fallback pattern above)
- `start_date` is omitted (no GitHub account creation date available)

---

## GitHub-specific gotchas

1. **Emails are not in member list responses.** `GET /enterprises/{enterprise}/members`
   and `GET /orgs/{org}/members` do not return email addresses. The sync calls
   `GET /users/{login}` for each member to retrieve their email. For an enterprise
   with 1,000 members, this adds ~1,000 extra API calls. At 10 req/s, expect ~2
   minutes of API work before the Snipe-IT sync begins.

2. **Email resolution uses three-tier fallback.** The sync resolves each member's
   email using a priority chain: (1) SAML NameID or SCIM username from
   `externalIdentities`, (2) org verified domain email from
   `organizationVerifiedDomainEmails`, (3) public GitHub profile email from
   `GET /users/{login}`. SAML/SCIM always wins even when the user also has a public
   profile email — the IdP identity is the authoritative match for Snipe-IT records.
   Verified domain email covers members who joined before SSO was enforced and have no
   SAML/SCIM identity. Users who cannot be resolved by any method are warned and
   skipped. All three GraphQL lookups require org admin access and are skipped in EMU
   enterprise mode (see gotcha #13).

3. **Enterprise slug vs enterprise name.** The `enterprise` config key is the
   URL slug (e.g. `acme-corp`), not the display name. It appears in enterprise
   URLs: `https://github.com/enterprises/acme-corp`. The slug is always lowercase
   with hyphens.

4. **Role is a request filter, not a response field.** The GitHub members API does
   not return a `role` field in the response body. To determine roles, the client
   makes two separate calls — one with `role=member` and one with `role=owner` (or
   `role=admin` for org mode) — and assigns the role based on which call returned
   the user.

5. **Enterprise members API is EMU-only.** `GET /enterprises/{enterprise}/members`
   only works for Enterprise Managed Users (EMU) tenants. Traditional GitHub Enterprise
   Cloud accounts (where users keep their personal GitHub accounts) must enumerate
   members via org APIs instead. Set `github.organizations` to the list of org slugs
   within your enterprise to use the org-based path. Outside collaborators and pending
   invitations are supported in both organization mode and enterprise+organizations mode.

6. **Pending invitations are org membership invitations, not repo invitations.**
   `GET /orgs/{org}/invitations` returns pending invitations for org membership.
   Repository-level outside-collaborator invitations (added directly to a repo)
   are returned by `GET /repos/{owner}/{repo}/invitations` and are not synced by
   this tool.

7. **Bot accounts are skipped.** GitHub API responses include a `type` field.
   Accounts with `type: "Bot"` are silently excluded from the sync.

8. **Deduplication by email.** When `include_outside_collaborators` and
   `include_pending_invitations` are both enabled, a user might appear in multiple
   lists. The sync deduplicates by lowercased email — the first occurrence wins
   (member > outside_collaborator > pending_invitation priority).

9. **PAT rate limits.** GitHub PATs are limited to 5,000 requests/hour. For large
   enterprises, the per-member profile lookup (`GET /users/{login}`) can approach
   this limit. If you hit rate limits, the client returns an error with the
   `Retry-After` value from the response header.

10. **EMU enterprises (Enterprise Managed Users).** EMU usernames have the format
    `username@enterpriseslug` and are provisioned via SCIM. Their public profile
    emails may differ from their enterprise identity email. If email matching fails
    for an EMU enterprise, ensure users have their corporate email set as their
    public GitHub email.

11. **SAML SSO enforcement requires PAT authorization per org.** If an organization
    enforces SAML single sign-on, every PAT must be explicitly authorized for that
    org in addition to having the right scopes. The API returns 403 with the message
    "Resource protected by organization SAML enforcement." Fix: go to
    github.com/settings/tokens → click the token → "Configure SSO" → "Authorize"
    next to the org. This must be done each time a new PAT is created, even if the
    previous PAT was already authorized.

12. **Checkin pass not affected by collaborator/pending flags.** The checkin pass
    checks all currently assigned seats against the combined active-email set from
    the current sync. If `include_outside_collaborators` was previously enabled and
    is now disabled, collaborator seats will be checked in on the next sync run.

13. **All GraphQL email lookups require org admin.** The three GraphQL queries used
    for email resolution (`samlIdentityProvider.externalIdentities`,
    `scimIdentity.username`, and `organizationVerifiedDomainEmails`) all require the
    PAT owner to be an org admin (owner or billing manager). Regular org members
    receive GraphQL-level errors or only their own identity. All three lookups degrade
    gracefully — they return empty maps rather than failing the sync — so a non-admin
    PAT will simply fall back to public profile emails, warning and skipping users
    whose emails are private.

---

## File structure

```
main.go
cmd/
  root.go        # cobra root, viper init, logging
  sync.go        # sync command
  test.go        # test command
internal/
  github/
    client.go    # GitHub REST API client (stdlib net/http + rate limiter)
  slack/
    client.go    # Slack webhook client (verbatim from CLAUDE.md)
  snipeit/
    client.go    # Snipe-IT API client (verbatim from CLAUDE.md)
  sync/
    syncer.go    # sync logic
    result.go    # Result struct
.github/
  workflows/
    release.yml
go.mod
go.sum
settings.example.yaml
README.md
CONTEXT.md
CLAUDE.md        # gitignored — shared *2snipe template
.gitignore
```
