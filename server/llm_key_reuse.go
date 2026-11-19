package server

import (
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// A stored LLM API key never goes back to the browser (api_key is json:"-", the
// UI only sees a hint). Several endpoints let a request leave api_key blank to
// reuse the stored one — test connection, list models, save profile, save the
// legacy global config — while taking base_url from the request. Unchecked, one
// authenticated request with base_url pointed at any host received the
// provider credential in a header. The rule is notify.PrepareConfigUpdate's:
// change the destination, re-enter the credential.
const errKeyDestinationChanged = "地址已变更：已保存的 API Key 只会发往原来的地址，请重新填写 API Key"

// keyDestination is where a key used with cfg is sent: the normalized base URL
// (ConfigFrom already trims slashes and endpoint suffixes), or — base URL left
// blank — the format's default endpoint.
func keyDestination(cfg agent.Config) string {
	if base := strings.TrimSpace(cfg.BaseURL); base != "" {
		return strings.ToLower(base)
	}
	if cfg.Provider() == "anthropic" {
		return "default:anthropic"
	}
	return "default:openai" // openai and openai-responses share api.openai.com
}

// sameKeyDestination reports whether a key stored for `stored` may be used for
// a request configured as `next`.
func sameKeyDestination(stored, next agent.Config) bool {
	return keyDestination(stored) == keyDestination(next)
}

// profileDestination normalizes a stored profile the way requests are normalized.
func profileDestination(p *db.LLMProfile) agent.Config {
	return agent.ConfigFrom(p.Format, p.Model, p.BaseURL, "", "")
}
