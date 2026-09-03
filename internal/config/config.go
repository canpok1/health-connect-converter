// Package config は種別ごとの取り込み・集計ルールを YAML から読む。
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// 時刻の持ち方の3類型。
const (
	LayoutInstant  = "instant"
	LayoutInterval = "interval"
	LayoutSeries   = "series"
)

var dailyFuncs = map[string]bool{
	"mean":  true,
	"min":   true,
	"max":   true,
	"sum":   true,
	"count": true,
}

// 日次集約で1日を決める基準。
const (
	DateBasisStart = "start"
	DateBasisEnd   = "end"
)

var dateBases = map[string]bool{
	DateBasisStart: true,
	DateBasisEnd:   true,
}

// Categories は Health Connect のデータカテゴリ名と、エクスポートDBの
// health_data_category_priority_table.health_data_category が持つ整数の対応。
// 整数は Android の HealthDataCategory の定数値。
var Categories = map[string]int{
	"activity":          1,
	"body_measurements": 2,
	"cycle_tracking":    3,
	"nutrition":         4,
	"sleep":             5,
	"vitals":            6,
	"wellness":          7,
}

var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

const durationValueName = "duration_min"

// ColumnConfig は1つの値列の読み方。
type ColumnConfig struct {
	Column string  `yaml:"column"`
	Scale  float64 `yaml:"scale"`
}

// TypeConfig は1種別の取り込み・集計ルール。
type TypeConfig struct {
	SourceTable     string                  `yaml:"source_table"`
	TimeLayout      string                  `yaml:"time_layout"`
	SeriesTable     string                  `yaml:"series_table"`
	Columns         map[string]ColumnConfig `yaml:"columns"`
	Window          string                  `yaml:"window"`
	Daily           []string                `yaml:"daily"`
	IncludeDuration bool                    `yaml:"include_duration"`
	// Category は Health Connect のデータカテゴリ名（Categories のキー）。
	// アプリの優先度はカテゴリ単位で設定されるため、Dedupe を使うなら必須。
	Category string `yaml:"category"`
	// Dedupe が真なら、日次集約の前に「同じ時間帯を複数アプリが書いている」
	// レコードを Health Connect のアプリ優先度で1つへ絞る。
	Dedupe bool `yaml:"dedupe"`
	// DateBasis は日次集約でレコードをどの日に数えるか。既定は "start"。
	// 日をまたぐ睡眠を起床日へ寄せる場合に "end" を指定する。
	DateBasis string `yaml:"date_basis"`
}

// Config は設定ファイル全体。
type Config struct {
	Types map[string]TypeConfig `yaml:"types"`
}

// Load は path の YAML を読み、検証して返す。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	for key, tc := range cfg.Types {
		for name, col := range tc.Columns {
			if col.Scale == 0 {
				col.Scale = 1
				tc.Columns[name] = col
			}
		}
		if tc.DateBasis == "" {
			tc.DateBasis = DateBasisStart
		}
		cfg.Types[key] = tc
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Types) == 0 {
		return fmt.Errorf("config: types must not be empty")
	}

	for key, tc := range c.Types {
		if !identifierRe.MatchString(key) {
			return fmt.Errorf("config: type %q: invalid type key (must match %s)", key, identifierRe.String())
		}
		if err := tc.validate(key); err != nil {
			return err
		}
	}

	return nil
}

func (tc TypeConfig) validate(key string) error {
	if tc.SourceTable == "" {
		return fmt.Errorf("config: type %q: source_table is required", key)
	}

	switch tc.TimeLayout {
	case LayoutInstant, LayoutInterval:
		if tc.SeriesTable != "" {
			return fmt.Errorf("config: type %q: series_table must be empty for time_layout %q", key, tc.TimeLayout)
		}
	case LayoutSeries:
		if tc.SeriesTable == "" {
			return fmt.Errorf("config: type %q: series_table is required for time_layout %q", key, LayoutSeries)
		}
	default:
		return fmt.Errorf("config: type %q: invalid time_layout %q (must be %s, %s, or %s)", key, tc.TimeLayout, LayoutInstant, LayoutInterval, LayoutSeries)
	}

	if len(tc.Columns) == 0 && !tc.IncludeDuration {
		return fmt.Errorf("config: type %q: columns must not be empty (unless include_duration is true)", key)
	}

	for name, col := range tc.Columns {
		if !identifierRe.MatchString(name) {
			return fmt.Errorf("config: type %q: invalid value name %q (must match %s)", key, name, identifierRe.String())
		}
		if name == durationValueName {
			return fmt.Errorf("config: type %q: value name %q is reserved", key, durationValueName)
		}
		if col.Column == "" {
			return fmt.Errorf("config: type %q: columns[%q].column is required", key, name)
		}
		if col.Scale < 0 {
			return fmt.Errorf("config: type %q: columns[%q].scale must not be negative", key, name)
		}
	}

	if _, _, err := tc.WindowDuration(); err != nil {
		return fmt.Errorf("config: type %q: %w", key, err)
	}

	if tc.Category != "" {
		if _, ok := Categories[tc.Category]; !ok {
			return fmt.Errorf("config: type %q: unknown category %q", key, tc.Category)
		}
	}
	if tc.Dedupe && tc.Category == "" {
		return fmt.Errorf("config: type %q: category is required when dedupe is true", key)
	}
	if tc.DateBasis != "" && !dateBases[tc.DateBasis] {
		return fmt.Errorf("config: type %q: invalid date_basis %q (must be %s or %s)", key, tc.DateBasis, DateBasisStart, DateBasisEnd)
	}
	if tc.DateBasis == DateBasisEnd && tc.TimeLayout == LayoutInstant {
		return fmt.Errorf("config: type %q: date_basis %q is meaningless for time_layout %q", key, DateBasisEnd, LayoutInstant)
	}

	if len(tc.Daily) == 0 {
		return fmt.Errorf("config: type %q: daily must not be empty", key)
	}
	seen := make(map[string]bool, len(tc.Daily))
	for _, fn := range tc.Daily {
		if !dailyFuncs[fn] {
			return fmt.Errorf("config: type %q: invalid daily function %q", key, fn)
		}
		if seen[fn] {
			return fmt.Errorf("config: type %q: duplicate daily function %q", key, fn)
		}
		seen[fn] = true
	}

	return nil
}

// TypeKeys は種別キーを辞書順で返す。
func (c *Config) TypeKeys() []string {
	keys := make([]string, 0, len(c.Types))
	for k := range c.Types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValueNames は Columns のキーを辞書順で返す。IncludeDuration が真なら末尾に
// "duration_min" を加える。
func (t TypeConfig) ValueNames() []string {
	names := make([]string, 0, len(t.Columns)+1)
	for name := range t.Columns {
		names = append(names, name)
	}
	sort.Strings(names)
	if t.IncludeDuration {
		names = append(names, durationValueName)
	}
	return names
}

var windowDaysRe = regexp.MustCompile(`^([0-9]+)d$`)

// WindowDuration は Window を解釈する。"all" のとき unlimited=true。
func (t TypeConfig) WindowDuration() (time.Duration, bool, error) {
	if t.Window == "all" {
		return 0, true, nil
	}
	m := windowDaysRe.FindStringSubmatch(t.Window)
	if m == nil {
		return 0, false, fmt.Errorf("invalid window %q (must be \"all\" or \"<N>d\")", t.Window)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false, fmt.Errorf("invalid window %q: %w", t.Window, err)
	}
	return time.Duration(n) * 24 * time.Hour, false, nil
}

// CategoryID は Category に対応する整数を返す。未設定・未知なら false。
func (t TypeConfig) CategoryID() (int, bool) {
	id, ok := Categories[t.Category]
	return id, ok
}
