# github2snipe

[![Latest Release](https://img.shields.io/github/v/release/jackvaughanjr/github2snipe)](https://github.com/jackvaughanjr/github2snipe/releases/latest) [![Go Version](https://img.shields.io/github/go-mod/go-version/jackvaughanjr/github2snipe)](go.mod) [![License](https://img.shields.io/github/license/jackvaughanjr/github2snipe)](LICENSE) [![Build](https://github.com/jackvaughanjr/github2snipe/actions/workflows/release.yml/badge.svg)](https://github.com/jackvaughanjr/github2snipe/actions/workflows/release.yml) [![Go Report Card](https://goreportcard.com/badge/github.com/jackvaughanjr/github2snipe)](https://goreportcard.com/report/github.com/jackvaughanjr/github2snipe) [![Downloads](https://img.shields.io/github/downloads/jackvaughanjr/github2snipe/total)](https://github.com/jackvaughanjr/github2snipe/releases)

Sync active GitHub Enterprise or Organization members into [Snipe-IT](https://snipeitapp.com/) as license seat assignments.

Supports both GitHub Enterprise (cloud) and standalone GitHub Organizations. Auth via a Personal Access Token (PAT). Runs fully headless — suitable for cron or CI.

> Part of the [\*2snipe](https://github.com/jackvaughanjr?tab=repositories&q=2snipe) integration family, inspired by [CampusTech](https://github.com/CampusTech)'s Snipe-IT integrations.

---

## How it works

On each sync run, `github2snipe`:

1. Fetches all active members from the configured GitHub Enterprise or Organization.
2. Optionally includes outside collaborators and pending invitations (org mode).
3. Resolves each member's email using a priority chain: SAML/SCIM identity from the
   org's SSO provider → org verified domain email (visible to org admins) → public
   GitHub profile email. SAML/SCIM always wins when present, even if a public profile
   email also exists — the IdP identity is the authoritative match for Snipe-IT records.
4. Finds or creates a matching Snipe-IT license record.
5. Checks out seats for active members; checks in seats for members who have left.
6. Writes member role and type into each seat's notes field.

Member role (owner, admin, member) and type (direct member, outside collaborator,
pending invitation) are recorded in Snipe-IT seat notes on checkout and updated
automatically when they change.

---

## Requirements

- Go 1.22+ (to build from source)
- A GitHub Personal Access Token — required scope depends on your GitHub Enterprise type:
  - **Enterprise mode (EMU tenants):** `admin:enterprise` scope; PAT owner must be an Enterprise Owner
  - **Enterprise mode (traditional GHEC):** `read:org` scope; set `github.organizations` in config
  - **Organization mode:** `read:org` scope
- A Snipe-IT instance with an API key that has license management permissions
- GitHub users must be resolvable by email: public GitHub profile email, SAML/SCIM
  identity, or org verified domain email. All three GraphQL lookups require the PAT
  owner to be an org admin; without admin access only public profile emails are used.

---

## Installation

**Download a pre-built binary** from the [latest release](https://github.com/jackvaughanjr/github2snipe/releases/latest):

    # macOS (Apple Silicon)
    curl -L https://github.com/jackvaughanjr/github2snipe/releases/latest/download/github2snipe-darwin-arm64 -o github2snipe
    chmod +x github2snipe

    # Linux (amd64)
    curl -L https://github.com/jackvaughanjr/github2snipe/releases/latest/download/github2snipe-linux-amd64 -o github2snipe
    chmod +x github2snipe

    # Linux (arm64)
    curl -L https://github.com/jackvaughanjr/github2snipe/releases/latest/download/github2snipe-linux-arm64 -o github2snipe
    chmod +x github2snipe

Or build from source:

    git clone https://github.com/jackvaughanjr/github2snipe
    cd github2snipe
    go build -o github2snipe .

---

## Configuration

Copy `settings.example.yaml` to `settings.yaml` and fill in your values:

```bash
cp settings.example.yaml settings.yaml
```

`settings.yaml` is gitignored and must never be committed. See `settings.example.yaml`
for all available options with inline documentation.

### Minimal configuration (enterprise mode — EMU tenants)

```yaml
github:
  mode: "enterprise"
  enterprise: "your-enterprise-slug"
  token: ""  # or set GITHUB_TOKEN env var; requires admin:enterprise scope

snipe_it:
  url: "https://your-snipe-it-instance.example.com"
  api_key: ""
  license_category_id: 0  # required
```

### Minimal configuration (enterprise mode — traditional GHEC)

```yaml
github:
  mode: "enterprise"
  enterprise: "your-enterprise-slug"
  organizations:
    - "your-org-slug"
    # - "another-org-slug"
  token: ""  # or set GITHUB_TOKEN env var; requires read:org scope

snipe_it:
  url: "https://your-snipe-it-instance.example.com"
  api_key: ""
  license_category_id: 0  # required
```

### Minimal configuration (organization mode)

```yaml
github:
  mode: "organization"
  organization: "your-org-name"
  token: ""  # or set GITHUB_TOKEN env var

snipe_it:
  url: "https://your-snipe-it-instance.example.com"
  api_key: ""
  license_category_id: 0  # required
```

### Environment variable overrides

| Variable        | Config key          |
|-----------------|---------------------|
| `GITHUB_TOKEN`  | `github.token`      |
| `SNIPE_URL`     | `snipe_it.url`      |
| `SNIPE_TOKEN`   | `snipe_it.api_key`  |
| `SLACK_WEBHOOK` | `slack.webhook_url` |

---

## Usage

### Validate connections

```bash
./github2snipe test
```

Reports the GitHub member count (by role) and current Snipe-IT license state
without making any changes.

### Run a sync

```bash
./github2snipe sync
```

### Dry run (no changes)

```bash
./github2snipe sync --dry-run
```

### Sync a single user

```bash
./github2snipe sync --email user@example.com
```

### Create Snipe-IT accounts for unmatched users

```bash
./github2snipe sync --create-users
```

### Force re-sync of all seat notes

```bash
./github2snipe sync --force
```

### Suppress Slack notifications for a run

```bash
./github2snipe sync --no-slack
```

### Global flags

| Flag              | Description                              |
|-------------------|------------------------------------------|
| `--config FILE`   | Path to config file (default: `settings.yaml`) |
| `-v, --verbose`   | INFO-level logging                       |
| `-d, --debug`     | DEBUG-level logging                      |
| `--log-file FILE` | Append logs to a file                    |
| `--log-format`    | `text` (default) or `json`               |
| `--version`       | Print version and exit                   |

---

## License naming

The Snipe-IT license name defaults to:

- `"GitHub Enterprise"` — when `mode: enterprise`
- `"GitHub"` — when `mode: organization`

Use `github.license_name_prefix` and `github.license_name_suffix` to distinguish
multiple GitHub tenants:

```yaml
github:
  license_name_prefix: "Acme - "
  # Result: "Acme - GitHub Enterprise"
```

---

## License seat count

In enterprise mode the sync automatically fetches the total purchased seat count
from GitHub via the `consumed-licenses` API and sets that on the Snipe-IT license.
This requires the PAT to have `read:enterprise` or `admin:enterprise` scope.

If auto-fetch is not available (org mode, or PAT lacks the required scope), set
`snipe_it.license_seats` as a manual override:

```yaml
snipe_it:
  license_seats: 34  # total purchased GitHub Enterprise licenses
```

The sync never shrinks seats. If the resolved seat count falls below the active
member count, the active member count is used as the floor.

---

## Seat notes

Each Snipe-IT seat checkout includes notes identifying the GitHub tenant and
the member's role or type:

| Member type            | Example notes                                                                              |
|------------------------|--------------------------------------------------------------------------------------------|
| Enterprise member      | `enterprise: acme-corp`<br>`role: member`<br>`github_login: agilemofo`                    |
| Enterprise owner       | `enterprise: acme-corp`<br>`role: owner`<br>`github_login: agilemofo`                     |
| Org member             | `organization: acme-corp`<br>`role: member`<br>`github_login: agilemofo`                  |
| Org admin              | `organization: acme-corp`<br>`role: admin`<br>`github_login: agilemofo`                   |
| Outside collaborator   | `organization: acme-corp`<br>`type: outside_collaborator`<br>`github_login: agilemofo`    |
| Pending (by login)     | `organization: acme-corp`<br>`status: pending_invitation`<br>`github_login: agilemofo`    |
| Pending (email-only)   | `organization: acme-corp`<br>`status: pending_invitation`                                 |

---

## Outside collaborators and pending invitations

Set the following in `settings.yaml` to include users who consume seats beyond
direct org members:

```yaml
github:
  include_outside_collaborators: true   # users with repo access but not org membership
  include_pending_invitations: true     # users with pending org membership invitations
```

Both are disabled by default. Supported in:
- **Organization mode** — queries the configured org directly.
- **Enterprise mode with `github.organizations` set** — queries each configured org
  and deduplicates across them.

Not supported in EMU enterprise mode (where `github.organizations` is empty),
because outside collaborators and pending invitations are org-level concepts not
exposed by the enterprise members API.

---

## Caveats

- **Private emails**: GitHub users with private email settings cannot be matched via
  their public profile. The sync resolves email through a three-tier fallback: (1) SAML
  NameID or SCIM username from the org's identity provider — typically the
  company-managed email for orgs using Okta, Azure AD, or Google Workspace; (2) org
  verified domain email, visible to org admins for members who joined before SSO was
  enforced; (3) public GitHub profile email. All three GraphQL lookups require the PAT
  owner to be an org admin. Users who cannot be resolved by any method are warned and
  skipped. None of these GraphQL lookups are available in EMU enterprise mode.
- **EMU vs traditional GHEC**: The enterprise members API (`GET /enterprises/{slug}/members`)
  only works for EMU (Enterprise Managed Users) tenants. Traditional GitHub Enterprise
  Cloud accounts must set `github.organizations` to enumerate members via org APIs.
  Using the wrong path results in a 404 from GitHub (not 403 — this is GitHub's
  intentional security behaviour).
- **SAML SSO enforcement**: If your organization enforces SAML SSO, the PAT must be
  explicitly SSO-authorized for each org in addition to having the correct scope. Go to
  github.com/settings/tokens → click your token → "Configure SSO" → "Authorize" next
  to the org. This must be repeated each time a new PAT is created.
- **Rate limits**: The sync makes one additional API call per member to resolve email
  addresses. For large enterprises (1,000+ members), allow a few minutes for the
  enrichment phase before Snipe-IT writes begin.

## Running with snipemgr (automated scheduling)

`github2snipe` can be scheduled via [snipemgr](https://github.com/jackvaughanjr/2snipe-manager), which manages installation, secret storage (GCP Secret Manager), and Cloud Run Job scheduling across all `*2snipe` integrations. A `Dockerfile` is included in this repo.

Build and push the container image to Artifact Registry — choose one method:

**Option A — Docker** (requires Docker installed and running):
```bash
gcloud auth configure-docker YOUR_REGION-docker.pkg.dev
docker build -t YOUR_REGION-docker.pkg.dev/YOUR_PROJECT/snipe-integrations/github2snipe:latest .
docker push YOUR_REGION-docker.pkg.dev/YOUR_PROJECT/snipe-integrations/github2snipe:latest
```

**Option B — Cloud Build** (no Docker required):
```bash
# One-time project setup:
gcloud services enable cloudbuild.googleapis.com --project=YOUR_PROJECT
PROJECT_NUMBER=$(gcloud projects describe YOUR_PROJECT --format='value(projectNumber)')
gcloud projects add-iam-policy-binding YOUR_PROJECT \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role="roles/cloudbuild.builds.builder"
gcloud projects add-iam-policy-binding YOUR_PROJECT \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role="roles/artifactregistry.writer"
gcloud projects add-iam-policy-binding YOUR_PROJECT \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role="roles/logging.logWriter"

# Build and push:
gcloud builds submit \
  --tag YOUR_REGION-docker.pkg.dev/YOUR_PROJECT/snipe-integrations/github2snipe:latest \
  --project=YOUR_PROJECT .
```

> These three IAM grants are required once per project. `cloudbuild.builds.builder` allows Cloud Build to read its source upload from Cloud Storage; `artifactregistry.writer` allows it to push the built image; `logging.logWriter` allows build logs to appear in Cloud Logging.

See the [snipemgr README](https://github.com/jackvaughanjr/2snipe-manager#building-container-images-for-cloud-run-jobs) for full GCP setup instructions (Artifact Registry repo creation, IAM, Cloud Scheduler).

---

## Version History

| Version | Key changes |
|---------|-------------|
| v1.6.0 | Make Snipe-IT API rate limit configurable via `sync.rate_limit_ms` and `SNIPE_RATE_LIMIT_MS` env var |
| v1.5.0 | Fixed seat state tracking; auto license seat count from GitHub API |
| v1.4.0 | Improved email resolution — SAML identity takes priority; SCIM and verified domain fallbacks added |
| v1.3.0 | Added `--no-slack` flag; SAML SSO email fallback for member enumeration |
| v1.2.1 | Documented EMU vs traditional GHEC split and syncer bug fix in CONTEXT.md |
| v1.2.0 | Support traditional GHEC orgs via `github.organizations` (in addition to EMU) |
| v1.1.0 | Detect SAML SSO 403 with actionable remediation instructions |
| v1.0.3 | Fixed enterprise PAT scope (`admin:enterprise` required, not `read:enterprise`) |
| v1.0.2 | Improved enterprise 404 error message explaining the owner slug requirement |
| v1.0.1 | Added `github_login` to seat notes |
| v1.0.0 | Initial scaffold — GitHub Enterprise Cloud → Snipe-IT license seat sync |
