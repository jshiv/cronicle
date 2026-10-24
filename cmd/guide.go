package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jshiv/cronicle/internal/cronicle"
)

var guideCmd = &cobra.Command{
	Use:   "guide",
	Short: "Print the guide to writing cronicle schedules (the AGENTS.md that init writes)",
	Long: `guide prints cronicle's usage and authoring guide: the commands, the
cronicle.hcl reference, and how to structure schedules as small inline
tasks joined by depends. cronicle init writes the same text to AGENTS.md.`,
	Example: `  cronicle guide
  cronicle guide > AGENTS.md`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprint(cmd.OutOrStdout(), cronicle.Guide)
	},
}

func init() {
	rootCmd.AddCommand(guideCmd)
}
