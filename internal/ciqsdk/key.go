package ciqsdk

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// KeyBits is the developer key size Connect IQ expects.
const KeyBits = 4096

// EnvKeyVar names the environment variable holding the human-chosen
// developer key path. It is the only key path allowed outside the project
// root, and it is only ever read, never written.
const EnvKeyVar = "GARMIN_MCP_KEY"

// ClientEnvHint ends a hint about GARMIN_MCP_KEY or JAVA_HOME: the server
// reads its own process env, which the MCP client sets, not the user's shell.
const ClientEnvHint = "in the env block of the MCP client's garmin-mcp entry (`garmin-mcp config install <client> --key <abs path> --java-home <abs path>` writes it), then restart the client; a shell export does not reach a client started outside that shell"

// EnvKey returns the developer key path from GARMIN_MCP_KEY when it is set to
// an absolute path naming an existing regular file.
func EnvKey() (path string, ok bool) {
	p := os.Getenv(EnvKeyVar)
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	st, err := os.Stat(p) //nolint:gosec // G703: the human sets this path on purpose; it is only stat'ed and read
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return filepath.Clean(p), true
}

// KeyInfo describes a developer key (never the key bytes).
type KeyInfo struct {
	Path        string   `json:"path"`
	Bits        int      `json:"bits"`
	Fingerprint string   `json:"public_key_sha256"`
	Mode        string   `json:"mode,omitempty"`
	OK          bool     `json:"ok"`
	Warnings    []string `json:"warnings,omitempty"`
}

// GenerateKey writes a new RSA-4096 PKCS#8 DER key to path with mode 0600.
// It refuses to overwrite an existing file.
func GenerateKey(path string) (KeyInfo, error) { return generateKey(path, KeyBits) }

func generateKey(path string, bits int) (KeyInfo, error) {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return KeyInfo{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return KeyInfo{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: callers pass a key path already confined to the project root; O_EXCL never overwrites
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return KeyInfo{}, fmt.Errorf("%s already exists; refusing to overwrite a developer key", path)
		}
		return KeyInfo{}, err
	}
	if _, err := f.Write(der); err != nil {
		f.Close()
		os.Remove(path)
		return KeyInfo{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return KeyInfo{}, err
	}
	return CheckKey(path)
}

// CheckKey validates a PKCS#8 DER RSA key: parse, size, permissions, and the
// SHA-256 fingerprint of the PKIX public key. A wrong size or loose
// permissions is reported (OK false / warning), a parse failure is an error.
func CheckKey(path string) (KeyInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return KeyInfo{}, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: callers pass a key path already confined to the project root
	if err != nil {
		return KeyInfo{}, err
	}
	pk, err := x509.ParsePKCS8PrivateKey(b)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("%s is not an unencrypted PKCS#8 DER key: %w", path, err)
	}
	rk, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return KeyInfo{}, fmt.Errorf("%s is a %T key; Connect IQ needs RSA", path, pk)
	}
	pub, err := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	if err != nil {
		return KeyInfo{}, err
	}
	sum := sha256.Sum256(pub)
	ki := KeyInfo{Path: path, Bits: rk.N.BitLen(), Fingerprint: hex.EncodeToString(sum[:]), Mode: fmt.Sprintf("%04o", st.Mode().Perm()), OK: true}
	if ki.Bits != KeyBits {
		ki.OK = false
		ki.Warnings = append(ki.Warnings, fmt.Sprintf("key is %d bits; Connect IQ expects %d", ki.Bits, KeyBits))
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		ki.Warnings = append(ki.Warnings, "key is readable by group/others; chmod 600")
	}
	return ki, nil
}
