// Package config loads and validates the declarative target list that tells
// Sonda which local services to sit in front of.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Durations are kept as strings in the struct and parsed during validation so a
// bad value produces a message naming the field instead of a YAML type error.
type Config struct {
	APIListen    string    `yaml:"api_listen"`
	Database     string    `yaml:"database"`
	MaxBodyBytes int64     `yaml:"max_body_bytes"`
	BufferSize   int       `yaml:"buffer_size"`
	Retention    Retention `yaml:"retention"`
	Targets      []Target  `yaml:"targets"`
}

type Retention struct {
	MaxCalls int    `yaml:"max_calls"`
	MaxAge   string `yaml:"max_age"`
	Interval string `yaml:"interval"`

	maxAge   time.Duration
	interval time.Duration
}

func (r Retention) MaxAgeDuration() time.Duration   { return r.maxAge }
func (r Retention) IntervalDuration() time.Duration { return r.interval }

type Target struct {
	Name     string `yaml:"name"`
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"`
	Protocol string `yaml:"protocol"`

	// DescriptorSet points at a compiled FileDescriptorSet
	// (`buf build -o x.binpb` or `protoc --descriptor_set_out`). Only needed
	// when the service does not serve reflection.
	DescriptorSet string `yaml:"descriptor_set"`

	// Reflection is a pointer so an unset value can default to on while an
	// explicit `reflection: false` still turns it off.
	Reflection *bool `yaml:"reflection"`
}

// ReflectionEnabled reports whether Sonda should ask the service for its
// schema. It defaults to on: asking costs one call and fails harmlessly.
func (t Target) ReflectionEnabled() bool {
	return t.Reflection == nil || *t.Reflection
}

const (
	ProtocolHTTP = "http"
	ProtocolGRPC = "grpc"

	defaultAPIListen    = "127.0.0.1:9000"
	defaultDatabase     = "sonda.db"
	defaultMaxBodyBytes = 256 << 10
	defaultBufferSize   = 1024
	defaultMaxCalls     = 50_000
	defaultMaxAge       = "24h"
	defaultInterval     = "1m"
)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// LoadOrDefaults reads the file when there is one and falls back to defaults
// when there is not.
//
// Projects live in the database now, so a missing configuration file is an
// ordinary first run rather than an error: the process still knows where to
// listen and how much of a body to keep, and the interface supplies the rest.
func LoadOrDefaults(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := &Config{}
		c.applyDefaults()
		if err := c.validateSettings(); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// ProjectNameFor names the seeded project after the directory the
// configuration lives in, which is nearly always the project's own name.
func ProjectNameFor(configPath string) string {
	dir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil || filepath.Base(dir) == "." {
		return "default"
	}
	name := filepath.Base(dir)
	if name == "" || name == string(filepath.Separator) {
		return "default"
	}
	return name
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo in a key is a startup error, not a silent default
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.APIListen == "" {
		c.APIListen = defaultAPIListen
	}
	if c.Database == "" {
		c.Database = defaultDatabase
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.BufferSize == 0 {
		c.BufferSize = defaultBufferSize
	}
	if c.Retention.MaxCalls == 0 {
		c.Retention.MaxCalls = defaultMaxCalls
	}
	if c.Retention.MaxAge == "" {
		c.Retention.MaxAge = defaultMaxAge
	}
	if c.Retention.Interval == "" {
		c.Retention.Interval = defaultInterval
	}
	for i := range c.Targets {
		if c.Targets[i].Protocol == "" {
			c.Targets[i].Protocol = ProtocolHTTP
		}
	}
}

func (c *Config) validate() error {
	if err := c.validateSettings(); err != nil {
		return err
	}
	return c.validateTargets()
}

// validateSettings covers what belongs to the process rather than to a project.
func (c *Config) validateSettings() error {
	if c.MaxBodyBytes < 0 {
		return fmt.Errorf("max_body_bytes must not be negative")
	}
	if c.BufferSize <= 0 {
		return fmt.Errorf("buffer_size must be greater than zero")
	}
	if c.Retention.MaxCalls <= 0 {
		return fmt.Errorf("retention.max_calls must be greater than zero")
	}

	var err error
	if c.Retention.maxAge, err = time.ParseDuration(c.Retention.MaxAge); err != nil {
		return fmt.Errorf("retention.max_age %q: %w", c.Retention.MaxAge, err)
	}
	if c.Retention.maxAge <= 0 {
		return fmt.Errorf("retention.max_age must be greater than zero")
	}
	if c.Retention.interval, err = time.ParseDuration(c.Retention.Interval); err != nil {
		return fmt.Errorf("retention.interval %q: %w", c.Retention.Interval, err)
	}
	if c.Retention.interval <= 0 {
		return fmt.Errorf("retention.interval must be greater than zero")
	}
	return nil
}

func (c *Config) validateTargets() error {
	names := map[string]bool{}
	listens := map[string]bool{c.APIListen: true}
	for i, t := range c.Targets {
		where := fmt.Sprintf("targets[%d]", i)
		if t.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if names[t.Name] {
			return fmt.Errorf("%s: duplicate name %q", where, t.Name)
		}
		names[t.Name] = true

		if t.Listen == "" {
			return fmt.Errorf("%s (%s): listen is required", where, t.Name)
		}
		if listens[t.Listen] {
			return fmt.Errorf("%s (%s): listen address %q is already in use", where, t.Name, t.Listen)
		}
		listens[t.Listen] = true

		if t.Protocol != ProtocolHTTP && t.Protocol != ProtocolGRPC {
			return fmt.Errorf("%s (%s): protocol %q is not supported, use %q or %q", where, t.Name, t.Protocol, ProtocolHTTP, ProtocolGRPC)
		}

		if t.Protocol != ProtocolGRPC && (t.DescriptorSet != "" || t.Reflection != nil) {
			return fmt.Errorf("%s (%s): descriptor_set and reflection only apply to %q targets", where, t.Name, ProtocolGRPC)
		}
		if t.DescriptorSet != "" {
			if _, err := os.Stat(t.DescriptorSet); err != nil {
				return fmt.Errorf("%s (%s): descriptor_set %q: %w", where, t.Name, t.DescriptorSet, err)
			}
		}

		// A missing scheme is the realistic mistake here, and url.Parse reports
		// it as an unhelpful "first path segment cannot contain colon". Both
		// cases collapse into the message that says what to fix.
		u, err := url.Parse(t.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s (%s): upstream %q must start with http:// or https://", where, t.Name, t.Upstream)
		}
		if u.Host == "" {
			return fmt.Errorf("%s (%s): upstream %q has no host", where, t.Name, t.Upstream)
		}
	}
	return nil
}

// UpstreamURL is safe to call only after the config passed validation.
func (t Target) UpstreamURL() *url.URL {
	u, _ := url.Parse(t.Upstream)
	return u
}
