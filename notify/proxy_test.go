package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 显式配置的通知代理：代理本身常在本机（127.0.0.1:7890 之类），必须能连；而投递目标
// 仍不能是本机 / 链路本地 / 云元数据地址。守卫默认开启（清掉 TestMain 打开的逃生口）。
func TestNotifyProxyIsUsedAndTargetsStayGuarded(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	t.Cleanup(func() { _ = SetProxy("") })

	var hits atomic.Int32
	var seenHost atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seenHost.Store(r.Host) // a forward proxy receives the absolute target URL
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	if err := SetProxy("ftp://proxy.example:21"); err == nil {
		t.Fatal("an ftp proxy was accepted")
	}
	if err := SetProxy(proxy.URL); err != nil { // http://127.0.0.1:<port> — a loopback proxy
		t.Fatal(err)
	}

	// A public-looking target goes through the (loopback) proxy.
	if _, err := doJSON(context.Background(), http.MethodPost, "http://hook.notify-test.invalid/send", nil, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("delivery through the proxy failed: %v", err)
	}
	if hits.Load() != 1 || seenHost.Load() != "hook.notify-test.invalid" {
		t.Fatalf("proxy hits=%d host=%v, want one request for the target host", hits.Load(), seenHost.Load())
	}

	// Internal targets are refused before the proxy is contacted.
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8787/api/health",
		"http://localhost:8787/api/health",
		"http://100.100.100.200/latest/meta-data/",
		"http://[::1]:8787/",
	} {
		_, err := doJSON(context.Background(), http.MethodPost, target, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "拒绝投递") {
			t.Errorf("%s: err=%v, want it refused", target, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("the proxy was contacted for a refused target (hits=%d)", hits.Load())
	}

	// Cleared: back to direct delivery, where the dial guard applies again.
	if err := SetProxy(""); err != nil {
		t.Fatal(err)
	}
	if proxiedClient.Load() != nil {
		t.Fatal("SetProxy(\"\") did not clear the proxy")
	}
}
