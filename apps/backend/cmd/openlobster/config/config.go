// Package config implements the "config" subcommand.
//
// Usage:
//
//	openlobster config get <key> [<key> ...]
//	openlobster config set <key> <value> [<key> <value> ...]
//
// The config file path is resolved from $OPENLOBSTER_CONFIG or the default
// data/openlobster.yaml. Both encrypted (OLENC1 prefix) and plain YAML files
// are supported transparently.
package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neirth/openlobster/internal/infrastructure/config"
	"github.com/spf13/viper"
)

// Run is the entry point for the "config" subcommand.
// args contains everything after "config" in os.Args.
func Run(args []string) {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgPath(), "path to openlobster.yaml")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openlobster config <get|set> [options] ...

Subcommands:
  get <key> [<key> ...]              Print one or more config values
  set <key> <value> [<key> <value>]  Write one or more config values

Options:
`)
		fs.PrintDefaults()
	}

	if len(args) == 0 {
		fs.Usage()
		os.Exit(1)
	}

	sub := args[0]
	// Parse flags after the subcommand name.
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(1)
	}
	rest := fs.Args()

	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: cannot resolve path %q: %v\n", *cfgPath, err)
		os.Exit(1)
	}

	switch sub {
	case "get":
		runGet(abs, rest)
	case "set":
		runSet(abs, rest)
	default:
		fmt.Fprintf(os.Stderr, "config: unknown subcommand %q\n", sub)
		fs.Usage()
		os.Exit(1)
	}
}

// runGet prints the value of each requested key.
func runGet(cfgPath string, keys []string) {
	if len(keys) == 0 {
		fmt.Fprintln(os.Stderr, "config get: at least one key required")
		os.Exit(1)
	}

	v := loadViper(cfgPath)
	for _, key := range keys {
		val := v.Get(key)
		if val == nil {
			fmt.Printf("%s = (not set)\n", key)
		} else {
			fmt.Printf("%s = %v\n", key, val)
		}
	}
}

// runSet writes key=value pairs to the config file.
// Values are cast to the appropriate type (bool, int, float, string) by Viper.
func runSet(cfgPath string, pairs []string) {
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		fmt.Fprintln(os.Stderr, "config set: arguments must be <key> <value> pairs")
		os.Exit(1)
	}

	v := loadViper(cfgPath)
	for i := 0; i < len(pairs); i += 2 {
		key, val := pairs[i], pairs[i+1]
		v.Set(key, val)
		fmt.Printf("  %s = %s\n", key, val)
	}

	if err := config.WriteEncryptedConfigFromSettings(v.AllSettings(), cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "config set: failed to write %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	fmt.Printf("saved → %s\n", cfgPath)
}

// loadViper reads cfgPath (decrypting if needed) into a fresh Viper instance.
func loadViper(cfgPath string) *viper.Viper {
	data, err := config.ReadConfigBytes(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: cannot read %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		fmt.Fprintf(os.Stderr, "config: cannot parse %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	return v
}

func defaultCfgPath() string {
	if v := os.Getenv("OPENLOBSTER_CONFIG"); v != "" {
		return v
	}
	return "data/openlobster.yaml"
}
