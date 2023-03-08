package cli

import (
	"github.com/coder/coder/cli/clibase"
)

func (r *RootCmd) templatePlan() *clibase.Cmd {
	return &clibase.Cmd{
		Use: "plan <directory>",
		Middleware: clibase.Chain(
			clibase.RequireNArgs(1),
		),
		Short: "Plan a template push from the current directory",
		Handler: func(inv *clibase.Invokation) error {
			return nil
		},
	}
}
