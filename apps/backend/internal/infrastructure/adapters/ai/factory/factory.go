// Package factory creates AI provider adapters from application configuration.
package factory

import (
	aianthropicadapter "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/anthropic"
	aidockermodelrunner "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/docker"
	aiollama "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/ollama"
	aiopenai "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/openai"
	aiopenaicompat "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/openaicompat"
	aiopenrouter "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/openrouter"
	aizenadapter "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/zen"
	"github.com/neirth/openlobster/internal/domain/ports"
	"github.com/neirth/openlobster/internal/infrastructure/config"
)

// MaxOutputTokens is the fixed output-token limit for AI completions (~2000 chars, fits Discord).
const MaxOutputTokens = 500

// BuildFromConfig creates the AIProviderPort that matches the first configured
// provider in cfg. Returns nil if no provider is configured.
func BuildFromConfig(cfg *config.Config) ports.AIProviderPort {
	var p ports.AIProviderPort
	switch {
	case cfg.Providers.OpenAI.APIKey != "" && cfg.Providers.OpenAI.APIKey != "YOUR_API_KEY_HERE":
		model := cfg.Providers.OpenAI.Model
		if model == "" {
			model = "gpt-4o"
		}
		if baseURL := cfg.Providers.OpenAI.BaseURL; baseURL != "" {
			p = aiopenai.NewAdapterWithEndpoint(baseURL, cfg.Providers.OpenAI.APIKey, model, MaxOutputTokens)
		} else {
			p = aiopenai.NewAdapter(cfg.Providers.OpenAI.APIKey, model, MaxOutputTokens)
		}
	case cfg.Providers.OpenRouter.APIKey != "" && cfg.Providers.OpenRouter.APIKey != "YOUR_API_KEY_HERE":
		model := cfg.Providers.OpenRouter.DefaultModel
		if model == "" {
			model = "openai/gpt-4o"
		}
		p = aiopenrouter.NewAdapter(cfg.Providers.OpenRouter.APIKey, model, MaxOutputTokens)
	case cfg.Providers.OpenAICompat.APIKey != "" &&
		cfg.Providers.OpenAICompat.APIKey != "YOUR_API_KEY_HERE" &&
		cfg.Providers.OpenAICompat.BaseURL != "":
		model := cfg.Providers.OpenAICompat.Model
		if model == "" {
			model = "default"
		}
		p = aiopenaicompat.NewAdapter(
			cfg.Providers.OpenAICompat.BaseURL,
			cfg.Providers.OpenAICompat.APIKey,
			model,
			MaxOutputTokens,
		)
	case cfg.Providers.Ollama.Endpoint != "":
		model := cfg.Providers.Ollama.DefaultModel
		if model == "" {
			model = "llama3"
		}
		p = aiollama.NewAdapterWithOptions(cfg.Providers.Ollama.Endpoint, cfg.Providers.Ollama.APIKey, model, MaxOutputTokens, cfg.Logging.Level)
	case cfg.Providers.Anthropic.APIKey != "" && cfg.Providers.Anthropic.APIKey != "YOUR_API_KEY_HERE":
		model := cfg.Providers.Anthropic.Model
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		p = aianthropicadapter.NewAdapter(cfg.Providers.Anthropic.APIKey, model, MaxOutputTokens)
	case cfg.Providers.OpenCode.APIKey != "" && cfg.Providers.OpenCode.APIKey != "YOUR_API_KEY_HERE":
		model := cfg.Providers.OpenCode.Model
		if model == "" {
			model = "kimi-k2.5"
		}
		p = aizenadapter.NewAdapter(cfg.Providers.OpenCode.APIKey, model, MaxOutputTokens)
	case cfg.Providers.DockerModelRunner.Endpoint != "":
		model := cfg.Providers.DockerModelRunner.DefaultModel
		if model == "" {
			model = "ai/mistral-nemo"
		}
		p = aidockermodelrunner.NewAdapter(cfg.Providers.DockerModelRunner.Endpoint, model, MaxOutputTokens)
	}
	return p
}

// ProviderName returns a human-readable label for the active AI provider in cfg.
func ProviderName(cfg *config.Config) string {
	switch {
	case cfg.Providers.OpenAI.APIKey != "" && cfg.Providers.OpenAI.APIKey != "YOUR_API_KEY_HERE":
		return "openai"
	case cfg.Providers.OpenRouter.APIKey != "" && cfg.Providers.OpenRouter.APIKey != "YOUR_API_KEY_HERE":
		return "openrouter"
	case cfg.Providers.Ollama.Endpoint != "":
		return "ollama"
	case cfg.Providers.Anthropic.APIKey != "" && cfg.Providers.Anthropic.APIKey != "YOUR_API_KEY_HERE":
		return "anthropic"
	case cfg.Providers.OpenCode.APIKey != "" && cfg.Providers.OpenCode.APIKey != "YOUR_API_KEY_HERE":
		return "opencode-zen"
	case cfg.Providers.DockerModelRunner.Endpoint != "":
		return "docker-model-runner"
	default:
		return "none"
	}
}
