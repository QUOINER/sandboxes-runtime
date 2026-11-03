// Package manifest validates pinned runtime inputs.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest describes native runtime pins.
type Manifest struct {
	Version    int                  `yaml:"version"`
	Platforms  []string             `yaml:"platforms"`
	Components map[string]Component `yaml:"components"`
	Patches    map[string]Patch     `yaml:"patches"`
	Images     map[string]Image     `yaml:"images"`
}

// Component is a pinned runtime dependency.
type Component struct {
	Version string `yaml:"version,omitempty"`
	Commit  string `yaml:"commit,omitempty"`
	Source  string `yaml:"source"`
	SHA512  string `yaml:"sha512,omitempty"`
}

// Patch is a checked-in patch against an open-source component.
type Patch struct {
	File   string `yaml:"file"`
	SHA256 string `yaml:"sha256"`
	Status string `yaml:"status"`
}

// Image is a pinned container image.
type Image struct {
	Ref    string `yaml:"ref"`
	Digest string `yaml:"digest"`
}

// Load reads a manifest from path.
func Load(path string) (Manifest, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- manifest path is an explicit build input
	if err != nil {
		return Manifest{}, err
	}
	return parseManifest(data)
}

func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return m, nil
}

// Verify checks required fields and patch digests relative to root.
func (m Manifest) Verify(root string) error {
	if m.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if err := m.verifyComponents(); err != nil {
		return err
	}
	if err := m.verifyPatches(root); err != nil {
		return err
	}
	return m.verifyImages()
}

func (m Manifest) verifyComponents() error {
	for _, name := range []string{"containerd", "nerdbox", "libkrun", "gvisor-tap-vsock"} {
		c, ok := m.Components[name]
		if !ok || c.Source == "" || (c.Version == "" && c.Commit == "") {
			return fmt.Errorf("component %s is not pinned", name)
		}
	}
	for name, component := range m.Components {
		if component.SHA512 == "" {
			continue
		}
		if len(component.SHA512) != 128 {
			return fmt.Errorf("component %s has invalid SHA-512 pin", name)
		}
		if _, err := hex.DecodeString(component.SHA512); err != nil {
			return fmt.Errorf("component %s has invalid SHA-512 pin: %w", name, err)
		}
	}
	return nil
}

func (m Manifest) verifyPatches(root string) error {
	names := make([]string, 0, len(m.Patches))
	for name := range m.Patches {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := m.Patches[name]
		if p.File == "" || p.SHA256 == "" {
			return fmt.Errorf("patch %s is incomplete", name)
		}
		data, err := os.ReadFile(filepath.Join(root, p.File)) // #nosec G304 -- patch path is pinned in the manifest
		if err != nil {
			return fmt.Errorf("read patch %s: %w", name, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, p.SHA256) {
			return fmt.Errorf("patch %s digest mismatch: got %s", name, got)
		}
	}
	return nil
}

func (m Manifest) verifyImages() error {
	for name, image := range m.Images {
		if image.Ref == "" || !strings.HasPrefix(image.Digest, "sha256:") {
			return fmt.Errorf("image %s is not pinned by digest", name)
		}
	}
	return nil
}
