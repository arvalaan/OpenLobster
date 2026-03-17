// Package migrate implements the "migrate" subcommand.
//
// It reads an OpenClaw configuration file and writes the equivalent
// OpenLobster configuration, preserving existing values and only overwriting
// fields that have a clear 1-to-1 mapping.
//
// OpenClaw uses camelCase YAML keys; OpenLobster uses snake_case.
// Only the channel-credential and a few agent-level fields are migrated
// because OpenClaw's WhatsApp backend (Baileys/QR) is incompatible with
// OpenLobster's (Meta Cloud API).
package migrate

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neirth/openlobster/internal/infrastructure/config"
	"github.com/spf13/viper"
)

// mapping is a single OpenClaw → OpenLobster key translation.
type mapping struct {
	src string // dot-path in the OpenClaw config (camelCase)
	dst string // dot-path in the OpenLobster config (snake_case)
}

// fieldMappings lists every supported translation.
// Only fields with a clear semantic equivalence are included.
var fieldMappings = []mapping{
	// Agent
	{"agent.name", "agent.name"},

	// Telegram
	{"channels.telegram.enabled", "channels.telegram.enabled"},
	{"channels.telegram.botToken", "channels.telegram.bot_token"},
	// fallback: some OpenClaw versions use snake_case
	{"channels.telegram.bot_token", "channels.telegram.bot_token"},

	// Discord — OpenClaw uses "token", OpenLobster uses "bot_token"
	{"channels.discord.enabled", "channels.discord.enabled"},
	{"channels.discord.token", "channels.discord.bot_token"},
	{"channels.discord.botToken", "channels.discord.bot_token"},

	// Slack
	{"channels.slack.enabled", "channels.slack.enabled"},
	{"channels.slack.botToken", "channels.slack.bot_token"},
	{"channels.slack.bot_token", "channels.slack.bot_token"},
	{"channels.slack.appToken", "channels.slack.app_token"},
	{"channels.slack.app_token", "channels.slack.app_token"},

	// WhatsApp — OpenClaw uses Baileys (QR-based), OpenLobster uses Meta Cloud API.
	// The credentials are incompatible; only enabled flag can be carried over.
	{"channels.whatsapp.enabled", "channels.whatsapp.enabled"},

	// Twilio
	{"channels.twilio.enabled", "channels.twilio.enabled"},
	{"channels.twilio.accountSid", "channels.twilio.account_sid"},
	{"channels.twilio.account_sid", "channels.twilio.account_sid"},
	{"channels.twilio.authToken", "channels.twilio.auth_token"},
	{"channels.twilio.auth_token", "channels.twilio.auth_token"},
	{"channels.twilio.fromNumber", "channels.twilio.from_number"},
	{"channels.twilio.from_number", "channels.twilio.from_number"},
}

// Run is the entry point for the "migrate" subcommand.
func Run(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	src := fs.String("from", "", "path to OpenClaw config file (required)")
	dst := fs.String("config", defaultCfgPath(), "path to OpenLobster config file to update")
	dryRun := fs.Bool("dry-run", false, "print what would be changed without writing")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openlobster migrate [options]

Reads an OpenClaw configuration file and applies the supported field
translations to the OpenLobster config. Existing OpenLobster values are
preserved unless explicitly overwritten by the migration.

Note: WhatsApp credentials cannot be migrated — OpenClaw uses Baileys (QR)
while OpenLobster uses the Meta Cloud API (phone_id + api_token).

Options:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if *src == "" {
		fmt.Fprintln(os.Stderr, "migrate: -from <openclaw-config> is required")
		fs.Usage()
		os.Exit(1)
	}

	srcAbs, err := filepath.Abs(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: cannot resolve -from path: %v\n", err)
		os.Exit(1)
	}
	dstAbs, err := filepath.Abs(*dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: cannot resolve -config path: %v\n", err)
		os.Exit(1)
	}

	srcV := readYAML(srcAbs)
	dstV := loadDst(dstAbs)

	applied := 0
	for _, m := range fieldMappings {
		val := srcV.Get(m.src)
		if val == nil {
			continue
		}
		// Skip placeholder strings
		if s, ok := val.(string); ok && isPlaceholder(s) {
			continue
		}
		if *dryRun {
			fmt.Printf("  would set  %-45s = %v\n", m.dst, val)
		} else {
			dstV.Set(m.dst, val)
			fmt.Printf("  %-45s = %v\n", m.dst, val)
		}
		applied++
	}

	if applied == 0 {
		fmt.Println("migrate: no mappable fields found in the OpenClaw config.")
		return
	}

	if *dryRun {
		fmt.Printf("\ndry-run: %d field(s) would be migrated to %s\n", applied, dstAbs)
		return
	}

	if err := config.WriteEncryptedConfigFromSettings(dstV.AllSettings(), dstAbs); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: failed to write %s: %v\n", dstAbs, err)
		os.Exit(1)
	}
	fmt.Printf("\nmigrated %d field(s) → %s\n", applied, dstAbs)
	printWhatsAppWarning()
}

// readYAML reads a plain YAML file (OpenClaw configs are not encrypted).
func readYAML(path string) *viper.Viper {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: cannot read %s: %v\n", path, err)
		os.Exit(1)
	}
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: cannot parse %s: %v\n", path, err)
		os.Exit(1)
	}
	return v
}

// loadDst loads the existing OpenLobster config (creating defaults if absent).
func loadDst(path string) *viper.Viper {
	data, err := config.ReadConfigBytes(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No existing config — start with an empty viper; config.Load will
			// bootstrap defaults on the next "serve" run.
			v := viper.New()
			v.SetConfigType("yaml")
			return v
		}
		fmt.Fprintf(os.Stderr, "migrate: cannot read %s: %v\n", path, err)
		os.Exit(1)
	}
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: cannot parse %s: %v\n", path, err)
		os.Exit(1)
	}
	return v
}

func isPlaceholder(s string) bool {
	placeholders := []string{
		"", "YOUR_API_KEY_HERE", "YOUR_BOT_TOKEN_HERE",
		"YOUR_ACCOUNT_SID", "YOUR_AUTH_TOKEN", "YOUR_API_TOKEN_HERE",
	}
	for _, p := range placeholders {
		if s == p {
			return true
		}
	}
	return false
}

func printWhatsAppWarning() {
	fmt.Println(`
  ⚠  WhatsApp credentials were NOT migrated.
     OpenClaw uses Baileys (QR-code / WhatsApp Web); OpenLobster uses the
     Meta Cloud API and requires a phone_id and api_token from
     developers.facebook.com. Set them manually:

       openlobster config set \
         channels.whatsapp.enabled true \
         channels.whatsapp.phone_id  <YOUR_PHONE_ID> \
         channels.whatsapp.api_token <YOUR_API_TOKEN>`)
}

func defaultCfgPath() string {
	if v := os.Getenv("OPENLOBSTER_CONFIG"); v != "" {
		return v
	}
	return "data/openlobster.yaml"
}
