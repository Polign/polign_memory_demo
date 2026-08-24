package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDataURL is the demo dataset artifact the embedding model ships in
// (shared with the polign wiki demo). Only the model files are used here; the
// Wikipedia passages in the same tarball are ignored.
const DefaultDataURL = "https://github.com/Polign/polign/releases/download/demo-data-v1/polign-demo-wiki-simple-v1.tar.gz"

// modelFiles are the artifact members the embedder needs.
var modelFiles = []string{"model.json", "vocab.txt", "embeddings.f32"}

// EnsureModel makes dir contain the embedding model files, downloading and
// unpacking the artifact from url (an http(s) URL or a local tarball path) if
// any are missing.
func EnsureModel(dir, url string, logf func(format string, args ...any)) error {
	missing := false
	for _, name := range modelFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("model dataset: %w", err)
	}

	tarball := url
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		logf("downloading embedding model (one-time) from %s", url)
		tmp, err := download(dir, url)
		if err != nil {
			return fmt.Errorf("model dataset: %w", err)
		}
		defer os.Remove(tmp)
		tarball = tmp
	}

	logf("unpacking model into %s", dir)
	if err := extractTarGz(tarball, dir, modelFiles); err != nil {
		return fmt.Errorf("model dataset: unpack %s: %w", url, err)
	}
	for _, name := range modelFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("model dataset: artifact is missing %s", name)
		}
	}
	return nil
}

func download(dir, url string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Get(url) //nolint:noctx // interactive CLI download; ^C kills the process
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "download-*.tar.gz")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// extractTarGz unpacks the named members of a flat tarball into dir, rejecting
// entry names that would escape it.
func extractTarGz(tarball, dir string, only []string) error {
	wanted := make(map[string]bool, len(only))
	for _, name := range only {
		wanted[name] = true
	}
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			return fmt.Errorf("unexpected entry %q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg || !wanted[name] {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted first-party artifact, bounded size
			_ = out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}
