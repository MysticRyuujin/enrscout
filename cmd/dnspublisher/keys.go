package main

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
)

const maxKeyPassphraseBytes = 8 << 10

// readKeyPassphrase strips only the first line's CR/LF terminator, preserving a passphrase's own surrounding spaces.
func readKeyPassphrase(path string) (string, error) {
	data, err := readPrivateFile(path, "key passphrase", maxKeyPassphraseBytes)
	if err != nil {
		return "", err
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSuffix(line, "\r"), nil
}

// readPrivateFile validates and reads the same opened file descriptor so a
// path replacement cannot change the file after its type and mode checks.
func readPrivateFile(path, label string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s %q is accessible by group or others; require mode 0600 or stricter", label, path)
	}
	if maxBytes <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", label, err)
		}
		return data, nil
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	// The descriptor can grow after fstat (and some virtual files report size
	// zero), so the bounded read and post-read check are intentionally retained.
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxBytes)
	}
	return data, nil
}

func loadKey(keyFile, keyPass string, dryRun bool) (*ecdsa.PrivateKey, error) {
	if keyFile != "" {
		data, err := readPrivateFile(keyFile, "signing key", 0)
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(data)
		// devp2p dns sign uses a Web3 keystore JSON; accept it so the reused key yields identical enrtree URLs.
		if len(trimmed) > 0 && trimmed[0] == '{' {
			key, err := keystore.DecryptKey(trimmed, keyPass)
			if err != nil {
				return nil, fmt.Errorf("decrypt keystore signing key: %w", err)
			}
			return key.PrivateKey, nil
		}
		return crypto.HexToECDSA(string(trimmed))
	}
	if !dryRun {
		return nil, errors.New("--key-file is required unless --dry-run")
	}
	slog.Warn("no --key-file; generating an ephemeral key (dry-run only, not stable)")
	return crypto.GenerateKey()
}
