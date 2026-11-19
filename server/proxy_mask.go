package server

import (
	"errors"
	"net/url"
	"strings"
)

// Proxy URLs may embed credentials (http://user:pass@host:port). They used to be
// returned verbatim by GET /api/settings and the LLM profile list — the one kind
// of stored secret the API echoed back, while API keys are only ever hinted.
// The password is now masked on the way out, and a value that comes back still
// masked means "keep the stored password".
//
// The mask uses unreserved characters only, so it survives URL parsing and
// re-encoding unchanged.
const proxyPasswordMask = "__masked__"

// errProxyDestinationChanged mirrors the API-key rule: a stored credential is
// only reused for the destination it was saved with.
var errProxyDestinationChanged = errors.New("代理地址已变更：已保存的代理密码只用于原来的代理，请重新填写密码")

// maskProxyURL hides the password of a proxy URL for display. A URL without a
// password (or one that does not parse) is returned unchanged.
func maskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); !has {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), proxyPasswordMask)
	return u.String()
}

// resolveMaskedProxy turns a proxy URL submitted by the client into the value to
// use. A URL whose password is the mask keeps the stored password, but only when
// it still names the same proxy (scheme, host, user) — otherwise the stored
// password would be sent to whatever host the request names. Anything else is
// returned as submitted (trimmed) for the caller's own validation.
func resolveMaskedProxy(incoming, stored string) (string, error) {
	incoming = strings.TrimSpace(incoming)
	u, err := url.Parse(incoming)
	if err != nil || u.User == nil {
		return incoming, nil
	}
	if pw, has := u.User.Password(); !has || pw != proxyPasswordMask {
		return incoming, nil
	}
	su, serr := url.Parse(strings.TrimSpace(stored))
	if serr != nil || su.User == nil {
		return "", errProxyDestinationChanged
	}
	storedPassword, has := su.User.Password()
	if !has || !strings.EqualFold(su.Scheme, u.Scheme) || !strings.EqualFold(su.Host, u.Host) || su.User.Username() != u.User.Username() {
		return "", errProxyDestinationChanged
	}
	u.User = url.UserPassword(u.User.Username(), storedPassword)
	return u.String(), nil
}

// resolveLLMRequestProxy resolves the proxy of an LLM request (test connection,
// list models, legacy global config). A masked password refers to the stored
// proxy of the profile being edited, or — without a profile — of the config
// currently applied. Unmasked values pass through untouched, without a lookup.
func (s *Server) resolveLLMRequestProxy(proxy string, profileID *int64) (string, error) {
	if !strings.Contains(proxy, proxyPasswordMask) {
		return proxy, nil
	}
	stored := ""
	if profileID != nil {
		if p, err := s.m.pg.ProfileByID(*profileID); err == nil && p != nil {
			stored = p.Proxy
		}
	} else {
		s.cfgMu.Lock()
		stored = s.llmCfg.Proxy
		s.cfgMu.Unlock()
	}
	return resolveMaskedProxy(proxy, stored)
}
