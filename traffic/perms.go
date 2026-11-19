package traffic

import (
	"log"
	"os"
	"path/filepath"
)

// caKeyFilename is go-mitmproxy's combined private key + certificate file. The
// library writes it with os.Create (0644 under a typical umask), so any local
// user could read the key and mint certificates that every client trusting this
// CA — which operators are told to install — would accept.
const caKeyFilename = "mitmproxy-ca.pem"

// restrictCapturePerms locks down what a capture leaves on disk: the CA private
// key, and the directories holding captured exchanges (cookies, Authorization
// headers, bodies). MkdirAll never changes an existing directory's mode, so this
// also repairs installs created by earlier builds. Best effort: a failure (e.g. a
// bind mount owned by another user) is logged, never fatal — the proxy still has
// to start. On Windows chmod only toggles the read-only bit, so this is a no-op
// there; access is governed by the inherited ACL.
func restrictCapturePerms(dir string) {
	for _, p := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(dir, "_ca", caKeyFilename), 0o600},
		{filepath.Join(dir, "_index"), 0o700},
		{filepath.Join(dir, "_blobs"), 0o700},
	} {
		if err := os.Chmod(p.path, p.perm); err != nil && !os.IsNotExist(err) {
			log.Printf("[traffic] 收紧权限失败 %s：%v", p.path, err)
		}
	}
}

// ensureBlob stores body at its content address unless an intact copy is there.
//
// The store is content-addressed, so the correct size is known: a file of any
// other size is a truncated leftover of an interrupted write. The old
// "skip if the file exists" check kept such a file forever — every later
// identical body skipped the write and Blob/BlobRange served the damaged bytes.
//
// The write goes to a temp file in the same directory and is renamed into place,
// so a crash or full disk can never leave a partial blob at the final path.
// Concurrent writers of the same body are safe: each renames identical content.
// A leftover temp file is not a "<hash>.bin", so blob GC removes it.
func ensureBlob(path string, body []byte) error {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Size() == int64(len(body)) {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".blob-*.tmp") // created 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
