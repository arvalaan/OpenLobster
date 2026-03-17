// openlobster — autonomous messaging agent daemon.
//
// Usage: openlobster <command> [flags]
//
// Commands:
//
//	serve    Start the HTTP server and all messaging adapters (default)
//	migrate  Migrate an OpenClaw config file to OpenLobster format
//	config   Read or write configuration keys in the YAML (respects encryption)
//	version  Print build version and exit
//
// # License
// See LICENSE in the root of the repository.
package main

import (
	"embed"
	"fmt"
	"os"

	cmdconfig "github.com/neirth/openlobster/cmd/openlobster/config"
	cmdmigrate "github.com/neirth/openlobster/cmd/openlobster/migrate"
	"github.com/neirth/openlobster/cmd/openlobster/serve"
	cmdversion "github.com/neirth/openlobster/cmd/openlobster/version"
)

// version is set at build time via -ldflags "-X main.version=x.y.z"
var version = "dev"

// public is the embedded frontend + static assets.
//
//go:embed all:public
var public embed.FS

func main() {
	// Disable Ollama SDK key-based auth; we use Bearer token via our own transport.
	if os.Getenv("OLLAMA_AUTH") == "" {
		os.Setenv("OLLAMA_AUTH", "false")
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		serve.New(version, public).Run()
	case "migrate":
		cmdmigrate.Run(os.Args[2:])
	case "config":
		cmdconfig.Run(os.Args[2:])
	case "version", "--version", "-v":
		cmdversion.Run(os.Stdout, version)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "openlobster: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(1)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `Usage: openlobster <command> [options]

Commands:
  serve              Start the server (default when no command is given)
  migrate [options]  Migrate an OpenClaw config to OpenLobster format
  config  <get|set>  Read or write config keys (encryption-aware)
  version            Print build version and exit

Run "openlobster <command> -h" for per-command help.
`)
}
