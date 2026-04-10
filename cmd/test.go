package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/jackvaughanjr/github2snipe/internal/github"
	"github.com/jackvaughanjr/github2snipe/internal/snipeit"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Validate API connections and report current state",
	RunE:  runTest,
}

func init() {
	rootCmd.AddCommand(testCmd)
}

func runTest(cmd *cobra.Command, args []string) error {
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

	ctx := context.Background()

	ghClient := github.NewClient(
		viper.GetString("github.token"),
		mode,
		viper.GetString("github.enterprise"),
		viper.GetString("github.organization"),
		viper.GetStringSlice("github.organizations"),
	)
	snipeClient := snipeit.NewClient(
		viper.GetString("snipe_it.url"),
		viper.GetString("snipe_it.api_key"),
	)

	// --- GitHub ---
	fmt.Println("=== GitHub ===")
	if err := ghClient.ValidateConnection(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "GitHub error: %v\n", err)
		return err
	}
	fmt.Println("Connection: OK")

	members, err := ghClient.ListMembers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GitHub error listing members: %v\n", err)
		return err
	}

	memberCount, ownerAdminCount := 0, 0
	for _, m := range members {
		if m.Role == "member" {
			memberCount++
		} else {
			ownerAdminCount++
		}
	}

	if mode == "enterprise" {
		fmt.Printf("Enterprise members: %d (members: %d, owners: %d)\n",
			len(members), memberCount, ownerAdminCount)
	} else {
		fmt.Printf("Organization members: %d (members: %d, admins: %d)\n",
			len(members), memberCount, ownerAdminCount)
	}

	if mode == "organization" && viper.GetBool("github.include_outside_collaborators") {
		collabs, err := ghClient.ListOutsideCollaborators(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub error listing outside collaborators: %v\n", err)
			return err
		}
		fmt.Printf("Outside collaborators: %d\n", len(collabs))
	}

	if mode == "organization" && viper.GetBool("github.include_pending_invitations") {
		pending, err := ghClient.ListPendingInvitations(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub error listing pending invitations: %v\n", err)
			return err
		}
		fmt.Printf("Pending invitations: %d\n", len(pending))
	}

	// --- Snipe-IT ---
	fmt.Println("\n=== Snipe-IT ===")
	licenseName := computedLicenseName(mode)

	lic, err := snipeClient.FindLicenseByName(ctx, licenseName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Snipe-IT error: %v\n", err)
		return err
	}
	if lic == nil {
		fmt.Printf("License %q: not found (will be created on first sync)\n", licenseName)
	} else {
		fmt.Printf("License %q: id=%d seats=%d free=%d\n",
			lic.Name, lic.ID, lic.Seats, lic.FreeSeatsCount)
	}

	return nil
}
