// Package serve — cobra command entry point.
//
// # License
// See LICENSE in the root of the repository.
package serve

import (
	"io/fs"

	"github.com/spf13/cobra"
)

// Command returns the cobra command for the "serve" subcommand.
func Command(version string, publicFS fs.FS) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server and all messaging adapters (default)",
		Run: func(cmd *cobra.Command, args []string) {
			New(version, publicFS).Run()
		},
	}
}
