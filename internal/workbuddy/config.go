package workbuddy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAutoCheckinTime = "09:00"
	AutoCheckinJitter      = 3 * 3600 // +/- 3 hours in seconds
)
const ConfigPathEnv = "CPA_WORKBUDDY_CONFIG_PATH"

// PluginConfig holds plugin-level runtime settings configured under
// plugins.configs.cpa-workbuddy or the embedded control panel.
type PluginConfig struct {
	CNModelPrefix      bool   `json:"cn_model_prefix" yaml:"cn_model_prefix"`
	IntlModelPrefix    bool   `json:"intl_model_prefix" yaml:"intl_model_prefix"`
	AutoCheckinEnabled bool   `json:"auto_checkin_enabled" yaml:"auto_checkin_enabled"`
	AutoCheckinTime    string `json:"auto_checkin_time" yaml:"auto_checkin_time"`
}

// ConfigField describes a plugin configuration field exposed in plugin.register metadata.
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default,omitempty"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description,omitempty"`
}

// DefaultPluginConfig returns the default WorkBuddy plugin configuration.
func DefaultPluginConfig() PluginConfig {
	return PluginConfig{
		CNModelPrefix:      true,
		IntlModelPrefix:    true,
		AutoCheckinEnabled: true,
		AutoCheckinTime:    DefaultAutoCheckinTime,
	}
}

// ConfigFields returns CPA Metadata.ConfigFields descriptors for management UIs.
func ConfigFields() []ConfigField {
	return []ConfigField{
		{
			Name:        "cn_model_prefix",
			Type:        "boolean",
			Default:     true,
			Description: "Prefix domestic WorkBuddy (CN) model IDs with wb/",
		},
		{
			Name:        "intl_model_prefix",
			Type:        "boolean",
			Default:     true,
			Description: "Prefix international WorkBuddy AI model IDs with wb-ai/",
		},
		{
			Name:        "auto_checkin_enabled",
			Type:        "boolean",
			Default:     true,
			Description: "Enable daily scheduled check-in (CN) and growth bonus report (Global)",
		},
		{
			Name:        "auto_checkin_time",
			Type:        "string",
			Default:     DefaultAutoCheckinTime,
			Description: "Daily anchor time (HH:MM local time) with +/-3h random jitter",
		},
	}
}

// ParseConfigYAML parses plugin YAML configuration onto DefaultPluginConfig.
func ParseConfigYAML(raw []byte) (PluginConfig, error) {
	cfg := DefaultPluginConfig()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return cfg, nil
	}
	if err := yaml.Unmarshal(trimmed, &cfg); err != nil {
		return PluginConfig{}, failure("invalid_config", fmt.Sprintf("invalid WorkBuddy plugin config YAML: %v", err), 400)
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg PluginConfig) (PluginConfig, error) {
	anchor := strings.TrimSpace(cfg.AutoCheckinTime)
	if anchor == "" {
		anchor = DefaultAutoCheckinTime
	}
	hour, minute, err := parseClockHHMM(anchor)
	if err != nil {
		return PluginConfig{}, err
	}
	cfg.AutoCheckinTime = fmt.Sprintf("%02d:%02d", hour, minute)
	return cfg, nil
}

// Config returns the active WorkBuddy plugin configuration.
func (s *Service) Config() PluginConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// DefaultConfigPath returns the standalone JSON config file path.
func DefaultConfigPath() string {
	if override := strings.TrimSpace(os.Getenv(ConfigPathEnv)); override != "" {
		return override
	}
	if testing.Testing() {
		return ""
	}
	return filepath.Join("data", "cpa-workbuddy", "config.json")
}

// ConfigPath returns the active standalone JSON config file path.
func (s *Service) ConfigPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.configPath
}

// SetConfigPath updates the standalone JSON config file path and loads it if present.
func (s *Service) SetConfigPath(path string) error {
	trimmed := strings.TrimSpace(path)
	s.cfgMu.Lock()
	s.configPath = trimmed
	s.cfgMu.Unlock()
	if trimmed == "" {
		return nil
	}
	loaded, ok, err := loadConfigFromFile(trimmed)
	if err != nil {
		return err
	}
	if ok {
		s.cfgMu.Lock()
		s.cfg = loaded
		s.cfgMu.Unlock()
		s.wakeScheduler()
	}
	return nil
}

// Reconfigure parses plugin YAML from CPA and updates active settings.
// If a standalone JSON config file exists, its values take precedence.
func (s *Service) Reconfigure(configYAML []byte) error {
	cfg, err := ParseConfigYAML(configYAML)
	if err != nil {
		return err
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	path := s.ConfigPath()
	if path != "" {
		if loaded, ok, loadErr := loadConfigFromFile(path); loadErr != nil {
			return loadErr
		} else if ok {
			normalized = loaded
		}
	}
	s.cfgMu.Lock()
	s.cfg = normalized
	s.cfgMu.Unlock()
	s.wakeScheduler()
	return nil
}

// UpdateConfig validates, persists to the standalone JSON config file,
// applies new plugin configuration, and wakes the scheduler.
func (s *Service) UpdateConfig(cfg PluginConfig) error {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	path := s.ConfigPath()
	if path != "" {
		if err := saveConfigToFile(path, normalized); err != nil {
			return err
		}
	}
	s.cfgMu.Lock()
	s.cfg = normalized
	s.cfgMu.Unlock()
	s.wakeScheduler()
	return nil
}

func loadConfigFromFile(path string) (PluginConfig, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PluginConfig{}, false, nil
		}
		return PluginConfig{}, false, err
	}
	cfg := DefaultPluginConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return PluginConfig{}, false, err
		}
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return PluginConfig{}, false, err
	}
	return normalized, true, nil
}

func saveConfigToFile(path string, cfg PluginConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (s *Service) modelPrefixEnabled(siteID string) bool {
	cfg := s.Config()
	switch siteID {
	case "workbuddy":
		return cfg.CNModelPrefix
	case "workbuddy-ai":
		return cfg.IntlModelPrefix
	default:
		return true
	}
}

func parseClockHHMM(value string) (int, int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 2 || len(parts[1]) != 2 {
		return 0, 0, failure("invalid_config", "auto_checkin_time must use 24-hour HH:MM format", 400)
	}
	hour, errH := strconv.Atoi(parts[0])
	minute, errM := strconv.Atoi(parts[1])
	if errH != nil || errM != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, failure("invalid_config", "auto_checkin_time must be a valid 24-hour time between 00:00 and 23:59", 400)
	}
	return hour, minute, nil
}
