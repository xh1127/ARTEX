package server

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// setupGate guards POST /api/auth/init with a one-time token.
//
// Until an admin password exists, init is unauthenticated by necessity, and the
// server listens on every interface by default. Without a gate, whoever reached a
// fresh install first — a scanner on the network, or a web page open in the
// operator's own browser — could set the password, receive the JWT and, through an
// MCP stdio entry, run commands on the host. The token is shown only where the
// operator alone can look: the server's console log and a file next to jwt.key.
type setupGate struct {
	mu        sync.Mutex
	token     string // normalized (lower-case, no separators); "" once retired
	display   string // grouped form shown to the operator
	path      string // keyDir/setup-token
	announced bool
}

const (
	setupTokenFilename = "setup-token"
	// No 0/o, 1/l/i look-alikes: the operator copies it from a log by eye.
	// 31 symbols × 16 characters ≈ 79 bits, far beyond online guessing.
	setupTokenAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	setupTokenLen      = 16
)

// newSetupGate generates a fresh token. It lives next to jwt.key (keyDir), never
// in dataDir: that one is the file manager's browsable workspace.
func newSetupGate(keyDir string) (*setupGate, error) {
	raw := make([]byte, setupTokenLen)
	for i := range raw {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(setupTokenAlphabet))))
		if err != nil {
			return nil, fmt.Errorf("generate setup token: %w", err)
		}
		raw[i] = setupTokenAlphabet[n.Int64()]
	}
	token := string(raw)
	groups := make([]string, 0, setupTokenLen/4)
	for i := 0; i < setupTokenLen; i += 4 {
		groups = append(groups, token[i:i+4])
	}
	return &setupGate{token: token, display: strings.Join(groups, "-"), path: filepath.Join(keyDir, setupTokenFilename)}, nil
}

// normalizeSetupToken lets the grouped form be pasted as-is: separators, spaces
// and case are ignored.
func normalizeSetupToken(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(s))
}

// announce prints the token and writes it next to jwt.key. Idempotent; it runs at
// startup and again whenever status reports "not initialized", which covers a
// database that was unreachable while the server started.
func (g *setupGate) announce() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.announced || g.token == "" {
		return
	}
	g.announced = true
	if err := os.WriteFile(g.path, []byte(g.display+"\n"), 0o600); err != nil {
		log.Printf("[auth] 写入初始化令牌文件失败: %v", err)
	}
	log.Printf("[auth] 尚未设置管理员密码。首次初始化令牌: %s", g.display)
	log.Printf("[auth] 打开 /setup 页面填入该令牌以设置密码（令牌同时写入了 %s）", g.path)
}

// matches reports whether input is the current, unretired token.
func (g *setupGate) matches(input string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	token := g.token
	g.mu.Unlock()
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(normalizeSetupToken(input)), []byte(token)) == 1
}

// retire disables the token for good and removes its file, once a password
// exists (after a successful init, or at startup when one was already set).
func (g *setupGate) retire() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.token, g.display = "", ""
	if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) {
		log.Printf("[auth] 删除初始化令牌文件失败: %v", err)
	}
}
