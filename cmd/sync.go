package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackvaughanjr/github2snipe/internal/github"
	"github.com/jackvaughanjr/github2snipe/internal/slack"
	"github.com/jackvaughanjr/github2snipe/internal/snipeit"
	"github.com/jackvaughanjr/github2snipe/internal/sync"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync active GitHub members into Snipe-IT license seats",
	RunE:  runSync,
}

func init() {
	rootCmd.AddCommand(syncCmd)

	syncCmd.Flags().Bool("dry-run", false, "simulate without making changes")
	syncCmd.Flags().Bool("force", false, "re-sync even if notes appear up to date")
	syncCmd.Flags().String("email", "", "sync a single user by email address")
	syncCmd.Flags().Bool("create-users", false, "create Snipe-IT accounts for GitHub users not already in Snipe-IT")

	_ = viper.BindPFlag("sync.dry_run", syncCmd.Flags().Lookup("dry-run"))
	_ = viper.BindPFlag("sync.force", syncCmd.Flags().Lookup("force"))
	_ = viper.BindPFlag("sync.create_users", syncCmd.Flags().Lookup("create-users"))
}

func runSync(cmd *cobra.Command, args []string) error {
	mode := viper.GetString("github.mode")
	if mode != "enterprise" && mode != "organization" {
		return fatal("github.mode must be \"enterprise\" or \"organization\", got %q", mode)
	}

	if mode == "enterprise" && viper.GetString("github.enterprise") == "" {
		return fatal("github.enterprise is required when mode is \"enterprise\"")
	}
	if mode == "organization" && viper.GetString("github.organization") == "" {
		return fatal("github.organization is required when mode is \"organization\"")
	}
	if viper.GetString("github.token") == "" {
		return fatal("github.token is required (or set GITHUB_TOKEN)")
	}

	categoryID := viper.GetInt("snipe_it.license_category_id")
	if categoryID == 0 {
		return fatal("snipe_it.license_category_id is required in settings.yaml")
	}

	licenseName := computedLicenseName(mode)

	ghClient := github.NewClient(
		viper.GetString("github.token"),
		mode,
		viper.GetString("github.enterprise"),
		viper.GetString("github.organization"),
	)
	snipeClient := snipeit.NewClient(
		viper.GetString("snipe_it.url"),
		viper.GetString("snipe_it.api_key"),
	)

	emailFilter, _ := cmd.Flags().GetString("email")

	cfg := sync.Config{
		DryRun:                      viper.GetBool("sync.dry_run"),
		Force:                       viper.GetBool("sync.force"),
		CreateUsers:                 viper.GetBool("sync.create_users"),
		LicenseName:                 licenseName,
		LicenseCategoryID:           categoryID,
		ManufacturerID:              viper.GetInt("snipe_it.license_manufacturer_id"),
		SupplierID:                  viper.GetInt("snipe_it.license_supplier_id"),
		Mode:                        mode,
		Enterprise:                  viper.GetString("github.enterprise"),
		Organization:                viper.GetString("github.organization"),
		IncludeOutsideCollaborators: viper.GetBool("github.include_outside_collaborators"),
		IncludePendingInvitations:   viper.GetBool("github.include_pending_invitations"),
	}

	if cfg.DryRun {
		slog.Info("dry-run mode enabled — no changes will be made")
	}

	slackClient := slack.NewClient(viper.GetString("slack.webhook_url"))
	ctx := context.Background()

	syncer := sync.NewSyncer(ghClient, snipeClient, cfg)
	result, err := syncer.Run(ctx, emailFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
		if !cfg.DryRun {
			msg := fmt.Sprintf("github2snipe sync failed: %v", err)
			if notifyErr := slackClient.Send(ctx, msg); notifyErr != nil {
				slog.Warn("slack notification failed", "error", notifyErr)
			}
		}
		return err
	}

	if !cfg.DryRun {
		for _, email := range result.UnmatchedEmails {
			msg := fmt.Sprintf("github2snipe: no Snipe-IT account found for GitHub user — %s", email)
			if notifyErr := slackClient.Send(ctx, msg); notifyErr != nil {
				slog.Warn("slack notification failed", "email", email, "error", notifyErr)
			}
		}

		msg := fmt.Sprintf(
			"github2snipe sync complete — checked out: %d, notes updated: %d, checked in: %d, skipped: %d, users created: %d, warnings: %d",
			result.CheckedOut, result.NotesUpdated, result.CheckedIn, result.Skipped, result.UsersCreated, result.Warnings,
		)
		if notifyErr := slackClient.Send(ctx, msg); notifyErr != nil {
			slog.Warn("slack notification failed", "error", notifyErr)
		}
	}

	fmt.Printf("Sync complete: checked_out=%d notes_updated=%d checked_in=%d skipped=%d users_created=%d warnings=%d\n",
		result.CheckedOut, result.NotesUpdated, result.CheckedIn, result.Skipped, result.UsersCreated, result.Warnings)
	return nil
}

// computedLicenseName returns the Snipe-IT license name based on config.
// If snipe_it.license_name is set, it is used verbatim.
// Otherwise: {github.license_name_prefix}{default}{github.license_name_suffix}
func computedLicenseName(mode string) string {
	if name := viper.GetString("snipe_it.license_name"); name != "" {
		return name
	}
	defaultBase := "GitHub Enterprise"
	if mode == "organization" {
		defaultBase = "GitHub"
	}
	return viper.GetString("github.license_name_prefix") + defaultBase + viper.GetString("github.license_name_suffix")
}
