// openlobster daemon entry point.
//
// All wiring, initialisation and lifecycle management is in the appinit
// package (cmd/openlobster/init/). main() only declares the embedded FS,
// sets a single environment default and delegates to appinit.New().Run().
//
// # License
// See LICENSE in the root of the repository.
package main

import (
	"embed"
	"os"

	appinit "github.com/neirth/openlobster/cmd/openlobster/init"
)

// version is set at build time via ldflags (-X main.version=...)
var version = "dev"

// public is the single embedded FS containing:
//
//	public/assets/     — compiled frontend (Vite outDir)
//	public/static/     — other static resources served at /static/
//
//go:embed all:public
var public embed.FS

func main() {
	// Disable Ollama SDK's key-based auth (~/.ollama/id_ed25519). We use Bearer
	// token (ollamaApiKey) via our own transport; the SDK auth is for ollama.com.
	if os.Getenv("OLLAMA_AUTH") == "" {
		os.Setenv("OLLAMA_AUTH", "false")
	}

	appinit.New(version, public).Run()
}
