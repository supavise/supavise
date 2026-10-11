package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// openGitHub builds the GitHub service over an open node, for the commands that set up the server's
// App. It holds no branching service: the commands never build a branch.
func openGitHub(cmd *cobra.Command, n *lifecycle.Node) (*github.Service, error) {
	opts := github.Options{Cfg: n.Cfg, Registry: n.Registry, Secrets: n.Secrets, Log: newLogger(n.Cfg)}
	opts.Audit = func(ctx context.Context, kind string, payload map[string]any) {
		_ = n.Registry.AppendEvent(ctx, "system", kind, payload)
	}
	// Removing the App takes its name out of Studio's unit, which reads it at start; creating one is
	// finished by the daemon, which serves GitHub's redirect and renders Studio itself.
	fm, err := fleet.NewManager(fleet.Deps{Cfg: n.Cfg, Log: newLogger(n.Cfg), Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts})
	if err != nil {
		return nil, err
	}
	opts.OnAppChange = func(ctx context.Context) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "Updating Studio's GitHub buttons...")
		if err := fm.RefreshStudio(ctx); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: Studio was not updated (%v); run `supavise fleet start` to apply it\n", err)
		}
		return nil
	}
	return github.New(opts)
}

func init() {
	ghCmd := &cobra.Command{
		Use:   "github",
		Short: "Connect projects to GitHub repositories: the server's GitHub App",
		Long: `The GitHub integration builds a preview branch from every pull request of a connected
repository, the way hosted Supabase does (docs/github-integration.md). It needs a GitHub App that
belongs to you and that GitHub can send webhooks to: https://api.<domain>/hooks/github.`,
	}
	app := &cobra.Command{Use: "app", Short: "Create, show or forget the server's GitHub App"}

	var owner, name string
	var replace bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Create the server's GitHub App through GitHub's manifest flow",
		Long: `Prints a link. Open it in a browser that is signed in to GitHub: GitHub shows the App (its name,
its permissions and the webhook URL of this server) for you to confirm, and sends the browser back
to this server, which stores the App's credentials sealed. Then install the App on the account or
organization that owns the repositories, and connect a repository in a project's Settings >
Integrations > GitHub.

The daemon (supavise.service) must be running: it serves the page the link opens and the address
GitHub sends the browser back to. The link is good once, for 30 minutes.

Without --owner the App belongs to the GitHub account that is signed in; with it, to that
organization. An App you already made is named in [github] of config.toml instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := openNode(cmd.Context())
			if err != nil {
				return err
			}
			defer n.Close()
			svc, err := openGitHub(cmd, n)
			if err != nil {
				return err
			}
			link, err := svc.BeginManifest(cmd.Context(), github.ManifestRequest{Owner: owner, Name: name, CreatedBy: "cli", Replace: replace})
			if err != nil {
				if errors.Is(err, registry.ErrReadOnly) {
					return errors.New("this server only reads the registry: run the command on the leader")
				}
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Open this link in a browser that is signed in to GitHub:\n\n  %s\n\n", link)
			fmt.Fprintf(w, "GitHub will send its webhooks to %s and the browser back to %s.\n", svc.WebhookURL(), n.Cfg.APIURL()+github.ManifestCallbackPath)
			fmt.Fprintln(w, "When it says the App is created, install it on your repositories and connect one in the project's settings.")
			return nil
		},
	}
	create.Flags().StringVar(&owner, "owner", "", "the GitHub organization that will own the App (default: the account that is signed in)")
	create.Flags().StringVar(&name, "name", "", "the App's name, at most 34 characters and unique on GitHub (default: Supavise <domain>)")
	create.Flags().BoolVar(&replace, "replace", false, "replace the App this server has; the users' authorizations go with it")

	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show how the server's GitHub App is set up",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := openNode(cmd.Context())
			if err != nil {
				return err
			}
			defer n.Close()
			svc, err := openGitHub(cmd, n)
			if err != nil {
				return err
			}
			st, err := svc.Status(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), st)
			}
			w := cmd.OutOrStdout()
			if !st.Configured {
				if st.Problem != "" {
					fmt.Fprintf(w, "The GitHub App is named in config.toml but cannot be used: %s\n", st.Problem)
				} else {
					fmt.Fprintln(w, "This server has no GitHub App. Create one with `supavise github app create`; the dashboard hides its GitHub buttons until then.")
				}
				fmt.Fprintf(w, "webhook URL:   %s\n", st.WebhookURL)
				return nil
			}
			fmt.Fprintf(w, "App:           %s (id %d, %s)\n", st.Name, st.AppID, st.Source)
			fmt.Fprintf(w, "slug:          %s\n", st.Slug)
			if st.Owner != "" {
				fmt.Fprintf(w, "owner:         %s\n", st.Owner)
			}
			fmt.Fprintf(w, "client id:     %s\n", st.ClientID)
			fmt.Fprintf(w, "install on:    %s\n", st.InstallURL)
			fmt.Fprintf(w, "webhook URL:   %s\n", st.WebhookURL)
			fmt.Fprintf(w, "callback URL:  %s\n", st.CallbackURL)
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	remove := &cobra.Command{
		Use:   "remove",
		Short: "Forget the App this server created (it stays on GitHub)",
		Long: `Removes the App's credentials and every user's authorization from this server. The
dashboard hides its GitHub buttons again. Connections to repositories are kept but do nothing
until an App exists; the App itself is deleted on GitHub (Settings > Developer settings > GitHub
Apps), which also ends its webhooks. Preview branches that pull requests made stay: delete them from
the Branches page. An App named in config.toml is removed by editing the file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := openNode(cmd.Context())
			if err != nil {
				return err
			}
			defer n.Close()
			svc, err := openGitHub(cmd, n)
			if err != nil {
				return err
			}
			had, err := svc.RemoveApp(cmd.Context())
			if err != nil {
				return err
			}
			if !had {
				fmt.Fprintln(cmd.OutOrStdout(), "This server has no GitHub App to remove.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "The GitHub App is forgotten. Delete it on GitHub too if you do not need it.")
			return nil
		},
	}
	app.AddCommand(create, status, remove)
	ghCmd.AddCommand(app)
	rootCmd.AddCommand(ghCmd)
}
