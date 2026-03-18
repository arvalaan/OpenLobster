package serve

import (
	"context"
	"log"
	"strings"

	"github.com/neirth/openlobster/internal/application/graphql/dto"
	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/domain/ports"
	domainhandlers "github.com/neirth/openlobster/internal/domain/handlers"
	msgrouter "github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/router"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/discord"
	mattermostadapter "github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/mattermost"
	slackadapter "github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/slack"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/telegram"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/twilio"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/whatsapp"
	"github.com/spf13/viper"
)

// channelCaps lists static capabilities per channel type used when reporting
// channel status to the dashboard after a hot-reload.
var channelCaps = map[string]dto.ChannelCapabilities{
	"telegram":   {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
	"discord":    {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
	"slack":      {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
	"whatsapp":   {HasVoiceMessage: true, HasCallStream: true, HasTextStream: true, HasMediaSupport: true},
	"twilio":     {HasVoiceMessage: true, HasCallStream: true, HasTextStream: true, HasMediaSupport: true},
	"mattermost": {HasVoiceMessage: false, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
}

// initChannels builds the messaging adapter list from config, populates
// chanReg, and creates the fan-out router.
func (a *App) initChannels() {
	cfg := a.Cfg
	log.Println("channels: initializing messaging adapters...")

	if !cfg.Channels.Telegram.Enabled {
		log.Println("channel: telegram — disabled (skipping)")
	} else if t := cfg.Channels.Telegram.BotToken; t == "" || t == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: telegram — no credentials configured (skipping)")
	} else if a2, err := telegram.NewAdapter(t); err != nil {
		log.Printf("channel: telegram — failed to initialize: %v", err)
	} else {
		a.MessagingAdapters = append(a.MessagingAdapters, a2)
		log.Println("channel: telegram — registered OK")
	}

	if !cfg.Channels.Discord.Enabled {
		log.Println("channel: discord — disabled (skipping)")
	} else if t := cfg.Channels.Discord.BotToken; t == "" || t == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: discord — no credentials configured (skipping)")
	} else if a2, err := discord.NewAdapter(t); err != nil {
		log.Printf("channel: discord — failed to initialize: %v", err)
	} else {
		a.MessagingAdapters = append(a.MessagingAdapters, a2)
		log.Println("channel: discord — registered OK")
	}

	if !cfg.Channels.Slack.Enabled {
		log.Println("channel: slack — disabled (skipping)")
	} else if bt := cfg.Channels.Slack.BotToken; bt == "" || bt == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: slack — no bot token configured (skipping)")
	} else if at := cfg.Channels.Slack.AppToken; at == "" || at == "YOUR_APP_TOKEN_HERE" {
		log.Println("channel: slack — no app-level token configured (skipping)")
	} else if a2, err := slackadapter.NewAdapter(bt, at); err != nil {
		log.Printf("channel: slack — failed to initialize: %v", err)
	} else {
		a.MessagingAdapters = append(a.MessagingAdapters, a2)
		log.Println("channel: slack — registered OK")
	}

	if !cfg.Channels.WhatsApp.Enabled {
		log.Println("channel: whatsapp — disabled (skipping)")
	} else if pid, tok := cfg.Channels.WhatsApp.PhoneID, cfg.Channels.WhatsApp.APIToken; pid == "" || tok == "" || tok == "YOUR_API_TOKEN_HERE" {
		log.Println("channel: whatsapp — no phone_id or api_token configured (skipping)")
	} else if a2, err := whatsapp.NewAdapter(pid, tok); err != nil {
		log.Printf("channel: whatsapp — failed to initialize: %v", err)
	} else {
		a.MessagingAdapters = append(a.MessagingAdapters, a2)
		log.Println("channel: whatsapp — registered OK")
	}

	if !cfg.Channels.Twilio.Enabled {
		log.Println("channel: twilio — disabled (skipping)")
	} else if sid, tok, from := cfg.Channels.Twilio.AccountSID, cfg.Channels.Twilio.AuthToken, cfg.Channels.Twilio.FromNumber; sid == "" || tok == "" || from == "" {
		log.Println("channel: twilio — no account_sid, auth_token or from_number configured (skipping)")
	} else {
		a.MessagingAdapters = append(a.MessagingAdapters, twilio.NewAdapter(sid, tok, from))
		log.Println("channel: twilio — registered OK")
	}

	// Mattermost: multi-profile — each profile gets its own adapter and registry key.
	if !cfg.Channels.Mattermost.Enabled {
		log.Println("channel: mattermost — disabled (skipping)")
	} else if len(cfg.Channels.Mattermost.Profiles) == 0 {
		log.Println("channel: mattermost — no profiles configured (skipping)")
	} else {
		mmSR := mattermostadapter.NewStickyRouter()
		for _, profile := range cfg.Channels.Mattermost.Profiles {
			p := profile
			ad, err := mattermostadapter.NewAdapter(cfg.Channels.Mattermost.ServerURL, p, mmSR)
			if err != nil {
				log.Printf("channel: mattermost[%s] — failed to initialize: %v", p.Name, err)
				continue
			}
			a.MessagingAdapters = append(a.MessagingAdapters, ad)
			a.MattermostProfileKeys = append(a.MattermostProfileKeys, ad.ChannelType())
			log.Printf("channel: mattermost[%s] — registered OK", p.Name)
		}
	}

	log.Printf("channels: %d adapter(s) active", len(a.MessagingAdapters))

	a.ChanReg = msgrouter.New()
	for _, adapter := range a.MessagingAdapters {
		switch ad := adapter.(type) {
		case *telegram.Adapter:
			a.ChanReg.Set("telegram", ad)
		case *discord.Adapter:
			a.ChanReg.Set("discord", ad)
		case *slackadapter.Adapter:
			a.ChanReg.Set("slack", ad)
		case *whatsapp.Adapter:
			a.ChanReg.Set("whatsapp", ad)
		case *twilio.Adapter:
			a.ChanReg.Set("twilio", ad)
		case *mattermostadapter.Adapter:
			a.ChanReg.Set(ad.ChannelType(), ad)
		}
	}

	a.MsgRouter = msgrouter.NewRouter(a.ChanReg)
}

// makeChannelMsgHandler returns a message-receive callback for the given
// channel type that delegates to the message handler.
func (a *App) makeChannelMsgHandler(ct string) func(context.Context, *models.Message) {
	return func(ctx context.Context, msg *models.Message) {
		if msg == nil || (msg.Content == "" && len(msg.Attachments) == 0 && msg.Audio == nil) {
			return
		}
		if hErr := a.MsgHandler.Handle(ctx, domainhandlers.HandleMessageInput{
			ChannelID:   msg.ChannelID,
			Content:     msg.Content,
			ChannelType: ct,
			SenderName:  msg.SenderName,
			SenderID:    msg.SenderID,
			IsGroup:     msg.IsGroup,
			IsMentioned: msg.IsMentioned,
			GroupName:   msg.GroupName,
			Attachments: msg.Attachments,
			Audio:       msg.Audio,
		}); hErr != nil {
			log.Printf("channel %s: message handler error: %v", ct, hErr)
		}
	}
}

// rebuildActiveChannels returns the current set of online channels by
// inspecting which adapters are registered in chanReg.
func (a *App) rebuildActiveChannels() []dto.ChannelStatus {
	var list []dto.ChannelStatus
	for _, t := range []string{"telegram", "discord", "slack", "whatsapp", "twilio"} {
		if a.ChanReg.Get(t) != nil {
			list = append(list, dto.ChannelStatus{
				ID: t, Name: t, Type: t, Status: "online",
				Enabled: true, Capabilities: channelCaps[t],
			})
		}
	}
	for _, key := range a.MattermostProfileKeys {
		if a.ChanReg.Get(key) != nil {
			name := strings.TrimPrefix(key, "mattermost:")
			list = append(list, dto.ChannelStatus{
				ID: key, Name: "mattermost:" + name, Type: "mattermost", Status: "online",
				Enabled: true, Capabilities: channelCaps["mattermost"],
			})
		}
	}
	return list
}

// reloadChannel stops the current adapter for channelType, re-creates it
// from viper config (so the reload is hot — no process restart needed) and
// starts its listener goroutine if the daemon is already running.
func (a *App) reloadChannel(channelType string) {
	a.ChanReg.Remove(channelType)
	enabled := viper.GetBool("channels." + channelType + ".enabled")

	var newAdapter ports.MessagingPort
	if enabled {
		switch channelType {
		case "telegram":
			if token := viper.GetString("channels.telegram.bot_token"); token != "" && token != "YOUR_BOT_TOKEN_HERE" {
				if ad, err := telegram.NewAdapter(token); err == nil {
					newAdapter = ad
				} else {
					log.Printf("channel: telegram — reload failed: %v", err)
				}
			}
		case "discord":
			if token := viper.GetString("channels.discord.bot_token"); token != "" && token != "YOUR_BOT_TOKEN_HERE" {
				if ad, err := discord.NewAdapter(token); err == nil {
					newAdapter = ad
				} else {
					log.Printf("channel: discord — reload failed: %v", err)
				}
			}
		case "slack":
			bt := viper.GetString("channels.slack.bot_token")
			at := viper.GetString("channels.slack.app_token")
			if bt != "" && bt != "YOUR_BOT_TOKEN_HERE" && at != "" && at != "YOUR_APP_TOKEN_HERE" {
				if ad, err := slackadapter.NewAdapter(bt, at); err == nil {
					newAdapter = ad
				} else {
					log.Printf("channel: slack — reload failed: %v", err)
				}
			}
		case "whatsapp":
			pid := viper.GetString("channels.whatsapp.phone_id")
			tok := viper.GetString("channels.whatsapp.api_token")
			if pid != "" && tok != "" && tok != "YOUR_API_TOKEN_HERE" {
				if ad, err := whatsapp.NewAdapter(pid, tok); err == nil {
					newAdapter = ad
				} else {
					log.Printf("channel: whatsapp — reload failed: %v", err)
				}
			}
		case "twilio":
			sid := viper.GetString("channels.twilio.account_sid")
			tok := viper.GetString("channels.twilio.auth_token")
			from := viper.GetString("channels.twilio.from_number")
			if sid != "" && tok != "" && from != "" {
				newAdapter = twilio.NewAdapter(sid, tok, from)
			}
		}
	}

	if newAdapter != nil {
		a.ChanReg.Set(channelType, newAdapter)
		if a.ChannelStartCtx != nil {
			switch ad := newAdapter.(type) {
			case *telegram.Adapter:
				go func() {
					if err := ad.Start(a.ChannelStartCtx, a.makeChannelMsgHandler("telegram")); err != nil {
						log.Printf("channel: telegram — listener failed (hot): %v", err)
					}
				}()
			case *discord.Adapter:
				go func() {
					if err := ad.Start(a.ChannelStartCtx, a.makeChannelMsgHandler("discord")); err != nil {
						log.Printf("channel: discord — listener failed (hot): %v", err)
					}
				}()
			case *slackadapter.Adapter:
				go func() {
					if err := ad.Start(a.ChannelStartCtx, a.makeChannelMsgHandler("slack")); err != nil {
						log.Printf("channel: slack — listener failed (hot): %v", err)
					}
				}()
			}
		}
		log.Printf("channel: %s — reloaded OK (hot)", channelType)
	} else if enabled {
		log.Printf("channel: %s — deactivated (no valid credentials)", channelType)
	} else {
		log.Printf("channel: %s — deactivated (disabled)", channelType)
	}

	if a.HTTPHandler != nil {
		a.HTTPHandler.UpdateAgentChannels(a.rebuildActiveChannels())
	}
}
