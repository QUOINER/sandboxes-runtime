// Package ctr provides the containerd CLI adapter used by the runtime backend.
package ctr

import "fmt"

// Config controls the containerd CLI adapter.
type Config struct {
	Socket      string
	Namespace   string
	CTR         string
	Runtime     string
	Snapshotter string
	Platform    string
}

func (c Config) withDefaults() (Config, error) {
	if c.Socket == "" {
		return Config{}, fmt.Errorf("containerd socket is required")
	}
	if c.CTR == "" {
		c.CTR = "ctr"
	}
	if c.Namespace == "" {
		c.Namespace = "rheic"
	}
	if c.Runtime == "" {
		c.Runtime = "io.containerd.nerdbox.v1"
	}
	if c.Snapshotter == "" {
		c.Snapshotter = "erofs"
	}
	if c.Platform == "" {
		c.Platform = "linux/arm64"
	}
	return c, nil
}
