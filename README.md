# github2snipe

Sync active GitHub Enterprise or Organization members into [Snipe-IT](https://snipeitapp.com/) as license seat assignments.

Supports both GitHub Enterprise (cloud) and standalone GitHub Organizations. Auth via a Personal Access Token (PAT). Runs fully headless — suitable for cron or CI.

---

## How it works

On each sync run, `github2snipe`:

1. Fetches all active members from the configured GitHub Enterprise or Organization.
2. Optionally includes outside collaborators and pending invitations (org mode).
3. Looks up each member's public GitHub profile to resolve their email address.
4. Finds or creates a matching Snipe-IT license record.
5. Checks out seats for active members; checks in seats for members who have left.
6. Writes member role and type into each seat's notes field.

Member role (owner, admin, member) and type (direct member, outside collaborator,
pending invitation) are recorded in Snipe-IT seat notes on checkout and updated
automatically when they change.

---

## Requirements

- Go 1.22+ (to build from source)
- A GitHub Personal Access Token with `read:enterprise` (enterprise mode) or `read:org` (org mode)
- A Snipe-IT instance with an API key that has license management permissions
- GitHub users must have a **public email** set in their GitHub profile for matching to work

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

### Minimal configuration (enterprise mode)

```yaml
github:
  mode: "enterprise"
  enterprise: "your-enterprise-slug"
  token: ""  # or set GITHUB_TOKEN env var

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

In organization mode, set the following in `settings.yaml` to include users
who consume seats beyond direct org members:

```yaml
github:
  include_outside_collaborators: true   # users with repo access but not org membership
  include_pending_invitations: true     # users with pending org membership invitations
```

Both are disabled by default. Has no effect in enterprise mode (outside collaborators
are an org-level concept not exposed by the enterprise API).

---

## Caveats

- **Private emails**: GitHub users with private email settings are skipped. Users
  must have a public email in their GitHub profile for the sync to process them.
- **Enterprise mode and outside collaborators**: The GitHub Enterprise API does not
  expose outside collaborators at the enterprise level. Use organization mode to
  track outside collaborators.
- **Rate limits**: The sync makes one additional API call per member to resolve email
  addresses. For large enterprises (1,000+ members), allow a few minutes for the
  enrichment phase before Snipe-IT writes begin.
