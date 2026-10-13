package cli

import (
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
	"time"
)

func newCustodyCmd() *cobra.Command {
	root := &cobra.Command{Use: "custody", Short: "Release custody or bind an existing publication destination"}
	for _, action := range []string{"release", "rebind", "reconcile", "validate"} {
		var destination, prURL, head string
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs, Short: "Explicitly quiesce and preserve the current branch's run", RunE: func(cmd *cobra.Command, args []string) error {
			env, err := openAxiDaemonEnv()
			if err != nil {
				return err
			}
			defer env.close()
			branch, err := git.CurrentBranch(cmd.Context(), ".")
			if err != nil {
				return err
			}
			if branch == "HEAD" {
				return fmt.Errorf("custody requires a checked-out branch")
			}
			runs, err := env.d.GetRunsByRepo(env.repo.ID)
			if err != nil {
				return err
			}
			id := ""
			for _, r := range runs {
				if r.Branch == branch {
					id = r.ID
					break
				}
			}
			if id == "" {
				return fmt.Errorf("no run for current branch")
			}
			var result ipc.CustodyResult
			if err := env.client.CallWithContext(cmd.Context(), ipc.MethodCustody, &ipc.CustodyParams{RepoID: env.repo.ID, Branch: branch, ExpectedRun: id, Action: action, Destination: destination, PRURL: prURL, Head: head}, &result, 3*time.Minute); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
		if action == "rebind" {
			cmd.Flags().StringVar(&destination, "branch", "", "existing destination branch (required)")
			cmd.Flags().StringVar(&prURL, "pr", "", "exact existing open PR URL (required)")
			_ = cmd.MarkFlagRequired("branch")
			_ = cmd.MarkFlagRequired("pr")
		}
		if action == "validate" {
			cmd.Short = "Start full validation of the exact archived rebind head"
			cmd.Flags().StringVar(&head, "head", "", "exact archived head returned by rebind (required)")
			_ = cmd.MarkFlagRequired("head")
		}
		root.AddCommand(cmd)
	}
	return root
}
