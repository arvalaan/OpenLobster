// Package version implements the "version" subcommand.
package version

import (
	"fmt"
	"io"
)

// Run prints the build version to w.
func Run(w io.Writer, v string) {
	fmt.Fprintf(w, "openlobster %s\n", v)
}
