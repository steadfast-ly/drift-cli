package cmd

import (
	"github.com/spf13/cobra"
)

// newCompletionCommand emits a shell completion script.
//
// Cobra's built-in completion command is disabled on the root and replaced with
// this one so that the shells offered match what DESIGN.md §5 commits to
// (bash, zsh, fish) rather than whatever cobra's default happens to include,
// and so the help text explains where to PUT the output — the step people
// actually get stuck on.
func newCompletionCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <bash|zsh|fish>",
		Short: "Emit a shell completion script",
		Long: `Emit a shell completion script on standard output.

  bash    drift completion bash > /etc/bash_completion.d/drift
          # or, per user:
          drift completion bash > ~/.local/share/bash-completion/completions/drift

  zsh     drift completion zsh > "${fpath[1]}/_drift"
          # ensure ` + "`autoload -U compinit && compinit`" + ` runs in ~/.zshrc

  fish    drift completion fish > ~/.config/fish/completions/drift.fish`,
		ValidArgs: []string{"bash", "zsh", "fish"},
		Args:      exactArgs(1, "one of bash, zsh, fish"),
		RunE: func(c *cobra.Command, args []string) error {
			root := c.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(app.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(app.Stdout)
			case "fish":
				return root.GenFishCompletion(app.Stdout, true)
			default:
				return usageErrorf("unsupported shell %q (want bash, zsh or fish)", args[0])
			}
		},
	}
	return cmd
}
