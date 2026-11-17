package main

import (
	"fmt"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func run(args []string) error {
	command := newRootCommand()
	command.SetArgs(args)
	return command.Execute()
}

func newRootCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "lazy-observe",
		Short:         "Self-hosted observability platform",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date),
		Args:          cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(
		newServerCommand(),
		newDoctorCommand(),
		newMCPCommand(),
		newPreStopCommand(),
	)
	return command
}

func newConfigFlagSet(name string) *pflag.FlagSet {
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	appconfig.BindFlags(flags)
	return flags
}
