package main

import "github.com/urfave/cli/v2"

func newPunchCommand() *cli.Command {
	return &cli.Command{
		Name:  "punch",
		Usage: "run the TCP hole-punching demo between two hosts",
		Subcommands: []*cli.Command{
			newPunchServerCommand(),
			newPunchClientCommand(),
		},
	}
}
