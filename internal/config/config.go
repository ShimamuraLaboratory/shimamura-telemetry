// Package config はエージェントの設定ファイル（TOML）を読み込む。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath は設定ファイルの既定の場所。
const DefaultPath = "/etc/shimamura-telemetry/config.toml"

// DefaultInterval は送信間隔の既定値。
const DefaultInterval = 5 * time.Second

// Config はエージェントの設定。
type Config struct {
	// ServerID は IdP 側で登録した計算機サーバーの ID。
	ServerID string `toml:"server_id"`
	// AgentToken は監視 Worker への接続に使うエージェントトークン。
	AgentToken string `toml:"agent_token"`
	// URL は監視 Worker の WebSocket エンドポイント（wss:// または ws://）。
	URL string `toml:"url"`
	// Interval は収集・送信の間隔。TOML では "5s" のような文字列で書く。
	Interval Duration `toml:"interval"`
}

// Duration は TOML の文字列（"5s" など）を time.Duration として読むための型。
type Duration struct {
	time.Duration
}

// UnmarshalText は encoding.TextUnmarshaler を実装する。
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	d.Duration = v
	return nil
}

// MarshalText は encoding.TextMarshaler を実装する。
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Load は path の設定ファイルを読み込み、既定値の補完と検証をして返す。
func Load(path string) (*Config, error) {
	cfg := &Config{}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("load config %s: unknown keys: %v", path, undecoded)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Interval.Duration == 0 {
		c.Interval.Duration = DefaultInterval
	}
}

// Validate は必須項目と値の範囲を検証する。
func (c *Config) Validate() error {
	var errs []error
	if c.ServerID == "" {
		errs = append(errs, errors.New("server_id is required"))
	}
	if c.AgentToken == "" {
		errs = append(errs, errors.New("agent_token is required"))
	}
	if c.URL == "" {
		errs = append(errs, errors.New("url is required"))
	} else if u, err := url.Parse(c.URL); err != nil {
		errs = append(errs, fmt.Errorf("url is invalid: %w", err))
	} else if u.Scheme != "wss" && u.Scheme != "ws" {
		errs = append(errs, fmt.Errorf("url scheme must be wss or ws, got %q", u.Scheme))
	} else if u.Host == "" {
		errs = append(errs, errors.New("url must include a host"))
	}
	if c.Interval.Duration < time.Second {
		errs = append(errs, fmt.Errorf("interval must be at least 1s, got %s", c.Interval.Duration))
	}
	return errors.Join(errs...)
}

// CheckPermissions は設定ファイルが所有者以外から読み書きできないことを確認する。
// 設定ファイルにはエージェントトークンが含まれるため、パーミッションは 0600 を想定する。
func CheckPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is accessible by group or others (mode %s); it should be 0600: %w",
			path, perm, fs.ErrPermission)
	}
	return nil
}
