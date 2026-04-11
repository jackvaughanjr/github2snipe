package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackvaughanjr/github2snipe/internal/github"
	"github.com/jackvaughanjr/github2snipe/internal/snipeit"
)

// Config controls sync behaviour.
type Config struct {
	DryRun            bool
	Force             bool
	CreateUsers       bool
	LicenseName       string
	LicenseCategoryID int
	// ManufacturerID is optional. If 0, "GitHub" is auto found/created.
	ManufacturerID int
	// SupplierID is optional. If 0, no supplier is set on the license.
	SupplierID                  int
	Mode                        string // "enterprise" or "organization"
	Enterprise                  string // enterprise slug (mode: enterprise)
	Organization                string // org name (mode: organization)
	IncludeOutsideCollaborators bool
	IncludePendingInvitations   bool
}

// Syncer orchestrates the GitHub → Snipe-IT license sync.
type Syncer struct {
	gh     *github.Client
	snipe  *snipeit.Client
	config Config
}

func NewSyncer(gh *github.Client, snipe *snipeit.Client, cfg Config) *Syncer {
	return &Syncer{gh: gh, snipe: snipe, config: cfg}
}

// Run executes the full sync. emailFilter restricts the checkout pass to one
// user (and skips the checkin pass entirely).
func (s *Syncer) Run(ctx context.Context, emailFilter string) (Result, error) {
	var result Result

	// 1. Validate GitHub API connection.
	slog.Info("validating GitHub API connection")
	if err := s.gh.ValidateConnection(ctx); err != nil {
		return result, err
	}

	// 2. Fetch all active members.
	slog.Info("fetching GitHub members")
	members, err := s.gh.ListMembers(ctx)
	if err != nil {
		return result, fmt.Errorf("listing GitHub members: %w", err)
	}
	slog.Info("fetched members", "count", len(members))

	// 3. Optionally add outside collaborators.
	// Works in organization mode and in enterprise mode when github.organizations is set.
	if s.config.IncludeOutsideCollaborators {
		slog.Info("fetching outside collaborators")
		collabs, err := s.gh.ListOutsideCollaborators(ctx)
		if err != nil {
			return result, fmt.Errorf("listing outside collaborators: %w", err)
		}
		slog.Info("fetched outside collaborators", "count", len(collabs))
		members = append(members, collabs...)
	}

	// 4. Optionally add pending invitations.
	// Works in organization mode and in enterprise mode when github.organizations is set.
	if s.config.IncludePendingInvitations {
		slog.Info("fetching pending invitations")
		pending, err := s.gh.ListPendingInvitations(ctx)
		if err != nil {
			return result, fmt.Errorf("listing pending invitations: %w", err)
		}
		slog.Info("fetched pending invitations", "count", len(pending))
		members = append(members, pending...)
	}

	// 5. Fetch SAML identities as an email fallback for users with private GitHub
	// profile emails. For orgs with SAML SSO, the NameID is typically the
	// company-managed email address. Returns an empty map (not an error) when
	// SAML is not configured or the PAT owner lacks org admin access.
	samlEmails, err := s.gh.GetSAMLIdentities(ctx)
	if err != nil {
		slog.Warn("could not fetch SAML identities — users with private profile emails will be skipped", "error", err)
		samlEmails = map[string]string{}
	}
	if len(samlEmails) > 0 {
		slog.Info("fetched SAML identities", "count", len(samlEmails))
	}

	// 6. Fetch organization verified domain emails as a second fallback.
	// For org members whose profile email is private and who have no SAML/SCIM
	// identity, GitHub exposes emails matching the org's verified domain(s) to
	// org admins via the organizationVerifiedDomainEmails GraphQL field.
	verifiedEmails, err := s.gh.GetVerifiedDomainEmails(ctx)
	if err != nil {
		slog.Warn("could not fetch verified domain emails", "error", err)
		verifiedEmails = map[string]string{}
	}
	if len(verifiedEmails) > 0 {
		slog.Info("fetched verified domain emails", "count", len(verifiedEmails))
	}

	// 7. Enrich members: fetch email, name, and createdAt from GitHub user profiles.
	// Members without a login (pending-by-email invitations) already have their
	// email set directly from the invitation response; skip profile lookup for them.
	slog.Info("enriching member profiles", "total", len(members))
	enriched := make([]github.Member, 0, len(members))
	for _, m := range members {
		if m.Login == "" {
			// Pending invitation by email — email is already set, name unknown.
			if m.Email != "" {
				enriched = append(enriched, m)
			} else {
				slog.Warn("skipping pending invitation with no email and no login")
			}
			continue
		}
		name, profileEmail, createdAt, err := s.gh.GetUserDetail(ctx, m.Login)
		if err != nil {
			slog.Warn("could not fetch user detail", "login", m.Login, "error", err)
			result.Warnings++
			continue
		}
		// Email resolution priority (highest to lowest):
		//   1. SAML/SCIM identity — asserted by IdP at login or provisioned via SCIM
		//   2. Org verified domain email — org-admin-visible email matching verified domain
		//   3. Public GitHub profile email — user's self-reported public email
		//   4. Skip with warning
		loginKey := strings.ToLower(m.Login)
		var email string
		if samlEmail, ok := samlEmails[loginKey]; ok {
			if profileEmail != "" && !strings.EqualFold(profileEmail, samlEmail) {
				slog.Info("overriding GitHub profile email with SAML identity",
					"login", m.Login, "profile_email", profileEmail, "saml_email", samlEmail)
			} else if profileEmail == "" {
				slog.Info("resolved email via SAML identity", "login", m.Login, "email", samlEmail)
			}
			email = samlEmail
		} else if vde, ok := verifiedEmails[loginKey]; ok {
			if profileEmail != "" && !strings.EqualFold(profileEmail, vde) {
				slog.Info("overriding GitHub profile email with verified domain email",
					"login", m.Login, "profile_email", profileEmail, "verified_email", vde)
			} else if profileEmail == "" {
				slog.Info("resolved email via verified domain", "login", m.Login, "email", vde)
			}
			email = vde
		} else if profileEmail != "" {
			email = profileEmail
		} else {
			slog.Warn("skipping GitHub user with private email — set a public email to include in sync",
				"login", m.Login)
			result.Warnings++
			continue
		}
		m.Name = name
		m.Email = email
		m.CreatedAt = createdAt
		enriched = append(enriched, m)
	}
	members = enriched

	// 8. Deduplicate by lowercased email (member > outside_collaborator > pending priority).
	seen := make(map[string]struct{}, len(members))
	deduped := make([]github.Member, 0, len(members))
	for _, m := range members {
		key := strings.ToLower(m.Email)
		if _, ok := seen[key]; ok {
			slog.Debug("deduplicating user", "email", key, "type", m.MemberType)
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, m)
	}
	members = deduped
	slog.Info("enriched and deduplicated members", "count", len(members))

	// 9. Build active email set for the checkin pass.
	activeEmails := make(map[string]struct{}, len(members))
	for _, m := range members {
		activeEmails[strings.ToLower(m.Email)] = struct{}{}
	}

	// 10. Apply --email filter.
	if emailFilter != "" {
		needle := strings.ToLower(emailFilter)
		filtered := members[:0]
		for _, m := range members {
			if strings.ToLower(m.Email) == needle {
				filtered = append(filtered, m)
				break
			}
		}
		members = filtered
		slog.Info("filtered to single user", "email", emailFilter, "found", len(members) > 0)
	}

	// 11. Resolve or auto-create the GitHub manufacturer in Snipe-IT.
	manufacturerID := s.config.ManufacturerID
	if !s.config.DryRun && manufacturerID == 0 {
		mfr, err := s.snipe.FindOrCreateManufacturer(ctx, "GitHub", "https://github.com")
		if err != nil {
			return result, fmt.Errorf("resolving GitHub manufacturer: %w", err)
		}
		manufacturerID = mfr.ID
		slog.Info("resolved manufacturer", "id", manufacturerID, "name", "GitHub")
	}

	// 12. Find or create the license.
	// Dry-run: find only; synthesize placeholder if not found (id=0).
	slog.Info("finding or creating license", "name", s.config.LicenseName)
	var lic *snipeit.License
	activeCount := len(activeEmails)
	if s.config.DryRun {
		lic, err = s.snipe.FindLicenseByName(ctx, s.config.LicenseName)
		if err != nil {
			return result, err
		}
		if lic == nil {
			slog.Info("[dry-run] license not found; would be created",
				"name", s.config.LicenseName, "seats", activeCount)
			lic = &snipeit.License{Name: s.config.LicenseName, Seats: activeCount}
		}
	} else {
		lic, err = s.snipe.FindOrCreateLicense(ctx, s.config.LicenseName, activeCount,
			s.config.LicenseCategoryID, manufacturerID, s.config.SupplierID)
		if err != nil {
			return result, err
		}
	}
	slog.Info("license resolved", "id", lic.ID, "seats", lic.Seats, "free", lic.FreeSeatsCount)

	// 13. Expand seats if needed (never shrink automatically).
	if activeCount > lic.Seats {
		slog.Info("expanding license seats", "current", lic.Seats, "needed", activeCount)
		if !s.config.DryRun {
			lic, err = s.snipe.UpdateLicenseSeats(ctx, lic.ID, activeCount)
			if err != nil {
				return result, err
			}
		}
	}

	// 14. Load current seat assignments.
	// Dry-run with a synthetic license (id=0) skips the API call.
	// In production, id=0 means something went wrong — fail fast.
	checkedOutByEmail := make(map[string]*snipeit.LicenseSeat)
	var freeSeats []*snipeit.LicenseSeat
	if lic.ID != 0 {
		slog.Info("loading current seat assignments")
		seats, err := s.snipe.ListLicenseSeats(ctx, lic.ID)
		if err != nil {
			return result, err
		}
		for i := range seats {
			seat := &seats[i]
			if seat.AssignedTo != nil && seat.AssignedTo.Email != "" {
				checkedOutByEmail[strings.ToLower(seat.AssignedTo.Email)] = seat
			} else {
				freeSeats = append(freeSeats, seat)
			}
		}
	} else if !s.config.DryRun {
		return result, fmt.Errorf("license resolved with id=0 in production mode — check Snipe-IT API permissions")
	} else {
		slog.Info("[dry-run] skipping seat load for new license")
	}
	slog.Info("seat state loaded", "checked_out", len(checkedOutByEmail), "free", len(freeSeats))

	// 15. Checkout / update loop.
	tenant := s.config.Enterprise
	if s.config.Mode == "organization" {
		tenant = s.config.Organization
	}

	for _, m := range members {
		email := strings.ToLower(m.Email)
		notes := buildNotes(m, s.config.Mode, tenant)

		snipeUser, err := s.snipe.FindUserByEmail(ctx, email)
		if err != nil {
			slog.Warn("error looking up Snipe-IT user", "email", email, "error", err)
			result.Warnings++
			continue
		}

		if snipeUser == nil {
			if !s.config.CreateUsers {
				slog.Warn("no Snipe-IT user found for GitHub user", "email", email)
				result.UnmatchedEmails = append(result.UnmatchedEmails, email)
				result.Warnings++
				continue
			}
			// Create the Snipe-IT user.
			firstName, lastName := splitName(m.Name, email)
			userNote := userCreationNote(m.MemberType)
			if s.config.DryRun {
				slog.Info("[dry-run] would create Snipe-IT user",
					"email", email, "type", m.MemberType)
				result.UsersCreated++
				result.CheckedOut++
				continue
			}
			created, err := s.snipe.CreateUser(ctx, firstName, lastName, email, email, userNote, m.CreatedAt)
			if err != nil {
				slog.Warn("failed to create Snipe-IT user", "email", email, "error", err)
				result.Warnings++
				continue
			}
			snipeUser = created
			result.UsersCreated++
		}

		if existing, ok := checkedOutByEmail[email]; ok {
			if existing.Notes == notes && !s.config.Force {
				slog.Debug("seat up to date", "email", email)
				result.Skipped++
				continue
			}
			slog.Info("updating seat notes", "email", email, "dry_run", s.config.DryRun)
			if !s.config.DryRun {
				if err := s.snipe.UpdateSeatNotes(ctx, lic.ID, existing.ID, notes); err != nil {
					slog.Warn("failed to update seat notes", "email", email, "error", err)
					result.Warnings++
					continue
				}
			}
			result.NotesUpdated++
			continue
		}

		if s.config.DryRun {
			slog.Info("[dry-run] would check out seat", "email", email, "notes", notes)
			result.CheckedOut++
			continue
		}
		if len(freeSeats) == 0 {
			slog.Warn("no free seats available", "email", email)
			result.Warnings++
			continue
		}
		seat := freeSeats[0]
		freeSeats = freeSeats[1:]

		slog.Info("checking out seat", "email", email, "seat_id", seat.ID)
		if err := s.snipe.CheckoutSeat(ctx, lic.ID, seat.ID, snipeUser.ID, notes); err != nil {
			slog.Warn("failed to checkout seat", "email", email, "error", err)
			freeSeats = append(freeSeats, seat) // return on failure
			result.Warnings++
			continue
		}
		result.CheckedOut++
	}

	// 16. Checkin loop — skip when --email filter is set.
	if emailFilter == "" {
		for email, seat := range checkedOutByEmail {
			if _, active := activeEmails[email]; active {
				continue
			}
			slog.Info("checking in seat for inactive user",
				"email", email, "seat_id", seat.ID, "dry_run", s.config.DryRun)
			if !s.config.DryRun {
				if err := s.snipe.CheckinSeat(ctx, lic.ID, seat.ID); err != nil {
					slog.Warn("failed to checkin seat", "email", email, "error", err)
					result.Warnings++
					continue
				}
			}
			result.CheckedIn++
		}
	}

	return result, nil
}

// buildNotes returns the formatted notes string written to the Snipe-IT seat.
func buildNotes(m github.Member, mode, tenant string) string {
	var lines []string
	if mode == "enterprise" {
		lines = append(lines, fmt.Sprintf("enterprise: %s", tenant))
	} else {
		lines = append(lines, fmt.Sprintf("organization: %s", tenant))
	}
	switch m.MemberType {
	case "member":
		if m.Role != "" {
			lines = append(lines, fmt.Sprintf("role: %s", m.Role))
		}
	case "outside_collaborator":
		lines = append(lines, "type: outside_collaborator")
	case "pending_invitation":
		lines = append(lines, "status: pending_invitation")
	}
	if m.Login != "" {
		lines = append(lines, fmt.Sprintf("github_login: %s", m.Login))
	}
	return strings.Join(lines, "\n")
}

// splitName derives first and last name from a GitHub display name or email.
// GitHub names are free-form; split on the first space. If no name is provided,
// fall back to the email local-part split on ".".
func splitName(name, email string) (first, last string) {
	if name != "" {
		parts := strings.SplitN(name, " ", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
		return parts[0], ""
	}
	local := strings.SplitN(email, "@", 2)[0]
	parts := strings.SplitN(local, ".", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return local, ""
}

// userCreationNote returns the Snipe-IT user notes string for auto-created users,
// indicating how the user was added to GitHub.
func userCreationNote(memberType string) string {
	switch memberType {
	case "outside_collaborator":
		return "Auto-created from GitHub via github2snipe (outside collaborator)"
	case "pending_invitation":
		return "Auto-created from GitHub via github2snipe (pending invitation)"
	default:
		return "Auto-created from GitHub via github2snipe"
	}
}
