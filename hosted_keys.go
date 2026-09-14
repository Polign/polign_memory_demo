package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// keyMinter creates one polign_db API key bound to a namespace. The server
// enforces that binding on every read and write, so a key minted for one
// visitor cannot reach another visitor's memories however it is used.
type keyMinter interface {
	Mint(ctx context.Context, namespace, note string) (string, error)
}

// apikeyCommand mints keys by running polign-apikey against the store, the
// same path an operator uses. It needs bucket credentials (on the demo host,
// the instance profile) and nothing from the running nodes; they pick the
// new record up from the store on their next cache miss.
type apikeyCommand struct {
	bin   string
	store string
}

func (c apikeyCommand) Mint(ctx context.Context, namespace, note string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, "-store", c.store, "create", "-namespace", namespace, "-note", note)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("polign-apikey create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "plgn_") {
			return line, nil
		}
	}
	return "", errors.New("polign-apikey create printed no key")
}

// keyring remembers minted keys on disk so a restart of this process keeps
// using each namespace's key instead of minting a fresh one per boot. The
// file holds secrets and is written with owner-only permissions.
type keyring struct {
	path   string
	minter keyMinter

	mu   sync.Mutex
	keys map[string]string
}

func loadKeyring(path string, minter keyMinter) (*keyring, error) {
	k := &keyring{path: path, minter: minter, keys: map[string]string{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &k.keys); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return k, nil
}

// Key returns the namespace's key, minting and persisting one on first use.
func (k *keyring) Key(ctx context.Context, namespace, note string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key := k.keys[namespace]; key != "" {
		return key, nil
	}
	key, err := k.minter.Mint(ctx, namespace, note)
	if err != nil {
		return "", err
	}
	k.keys[namespace] = key
	if err := k.save(); err != nil {
		return "", err
	}
	return key, nil
}

func (k *keyring) save() error {
	raw, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(k.path), ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), k.path)
}
