package traffic

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureBlobWritesRepairsAndSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadbeef.bin")
	body := bytes.Repeat([]byte("captured-body "), 64)

	if err := ensureBlob(path, body); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
		t.Fatal("new blob not written intact")
	}

	// A truncated leftover from an interrupted write used to be kept forever.
	if err := os.WriteFile(path, body[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBlob(path, body); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
		t.Fatalf("truncated blob was not repaired: %d bytes", len(got))
	}

	// An intact blob is left alone (no rewrite).
	before, _ := os.Stat(path)
	if err := ensureBlob(path, body); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("intact blob was rewritten")
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("blob mode=%v, want 0600 (captured bodies hold credentials)", fi.Mode().Perm())
		}
	}
}

// go-mitmproxy writes its CA key with os.Create (0644): any local user could read
// it and forge certificates trusted wherever this CA was installed. Open must
// lock the key and the capture directories down, including on existing installs.
func TestOpenRestrictsCAKeyAndCaptureDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits; Windows access is governed by ACLs")
	}
	dir := t.TempDir()
	// Simulate an install created by an earlier build: world-readable dirs.
	for _, d := range []string{"_index", "_blobs", "_ca"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "_ca", caKeyFilename): 0o600,
		filepath.Join(dir, "_index"):             0o700,
		filepath.Join(dir, "_blobs"):             0o700,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode=%v, want %v", path, got, want)
		}
	}
	// The public certificate must stay readable: clients import it to trust us.
	if fi, err := os.Stat(tr.CACertPath()); err != nil || fi.Mode().Perm()&0o044 == 0 {
		t.Fatalf("public CA certificate should remain readable: %v %v", fi, err)
	}
}
