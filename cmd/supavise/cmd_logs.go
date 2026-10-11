package main

import (
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/svclog"
	"github.com/supavise/supavise/internal/units"
)

func init() {
	var service, since string
	var lines int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <ref>",
		Short: "Show the logs of one project from the journal",
		Long: `Prints the most recent log lines of a project, oldest first, each with its time and the service
it came from. This is where a node keeps logs: the dashboard has no log store, so its Logs pages
and the MCP server's log tools say so and point here.

A project runs its own Postgres, Auth and REST units. Realtime, Storage, the pooler and the Edge
Runtime serve every project; their lines are shown when they mention the project's ref. Without
--service the command merges all of them. The ref ` + "`system`" + ` shows the dashboard's own Postgres and Auth.

--service takes ` + strings.Join(svclog.Names(), ", ") + `.
--since takes an age (30m, 2h, 1d) or a time (2026-10-10T12:00:00Z, in UTC).

The command reads the systemd journal, which only root and members of the systemd-journal
group may read: run it with sudo. With the exec backend (development) it prints the end of each
unit's log file, and --since and --follow are not available.`,
		Example: `  sudo supavise logs abcdefghijklmnopqrst
  sudo supavise logs abcdefghijklmnopqrst --service postgres -n 500 --since 1h
  sudo supavise logs abcdefghijklmnopqrst -f`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			o := svclog.Options{
				Ref: a[0], Service: service, Lines: lines, Follow: follow, Since: since,
				Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), Journal: svclog.ExecJournal{},
				Root: os.Geteuid() == 0,
			}
			if cfg.Supervisor == config.SupervisorExec {
				ex := units.NewExec(cfg, nil)
				o.Tail = ex.Tail
			}
			return svclog.Run(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVarP(&service, "service", "s", "", "show one service only ("+strings.Join(svclog.Names(), ", ")+")")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "how many of the most recent lines to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines until interrupted")
	cmd.Flags().StringVar(&since, "since", "", "only lines newer than this: an age (30m, 2h, 1d) or a time (2026-10-10T12:00:00Z)")
	rootCmd.AddCommand(cmd)
}
