package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestKeyringMintsOncePerNamespaceAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	minter := &fakeMinter{}
	ring, err := loadKeyring(path, minter)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ring.Key(context.Background(), "memdemo:abc", "visitor")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ring.Key(context.Background(), "memdemo:abc", "visitor")
	other, _ := ring.Key(context.Background(), "memdemo:def", "visitor")
	if first != again || first == other || len(minter.made) != 2 {
		t.Fatalf("keys: %q %q %q minted %v", first, again, other, minter.made)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	// A restart reads the file back instead of minting.
	reloaded, err := loadKeyring(path, &fakeMinter{})
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := reloaded.Key(context.Background(), "memdemo:abc", "visitor"); key != first {
		t.Fatalf("reloaded key = %q, want %q", key, first)
	}
}

func TestApikeyCommandParsesTheKeyLine(t *testing.T) {
	script := filepath.Join(t.TempDir(), "polign-apikey")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'key id:  abc'\necho 'namespace: '\"$6\"\necho\necho plgn_abc_secret\necho\necho 'This is the only time the full key is shown; only its hash is stored.'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := apikeyCommand{bin: script, store: "fs:/tmp/x"}.Mint(context.Background(), "memdemo:abc", "visitor")
	if err != nil || key != "plgn_abc_secret" {
		t.Fatalf("key = %q err = %v", key, err)
	}
	if _, err := (apikeyCommand{bin: "/nonexistent/polign-apikey", store: "fs:/tmp/x"}).Mint(context.Background(), "memdemo:abc", "visitor"); err == nil {
		t.Fatal("missing binary was not an error")
	}
}
