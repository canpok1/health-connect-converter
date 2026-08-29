package config

import (
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, yamlContent string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := writeFile(path, yamlContent); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_RepoConfig(t *testing.T) {
	// リポジトリ直下の config.yaml が実際に読めることを確認する。
	cfg, err := Load(repoConfigPath(t))
	if err != nil {
		t.Fatalf("Load repo config.yaml: %v", err)
	}
	if len(cfg.Types) == 0 {
		t.Fatalf("expected at least one type")
	}
}

func TestLoad_Valid(t *testing.T) {
	path := writeConfig(t, `
types:
  blood_pressure:
    source_table: blood_pressure_record_table
    time_layout: instant
    columns:
      systolic:
        column: systolic
      diastolic:
        column: diastolic
    window: all
    daily: [mean, count]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tc := cfg.Types["blood_pressure"]
	if tc.Columns["systolic"].Scale != 1 {
		t.Errorf("expected default scale 1, got %v", tc.Columns["systolic"].Scale)
	}
}

func TestLoad_ScaleDefaultsToOne(t *testing.T) {
	path := writeConfig(t, `
types:
  weight:
    source_table: weight_record_table
    time_layout: instant
    columns:
      weight_kg:
        column: weight
        scale: 0.001
      note:
        column: note
    window: all
    daily: [mean]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tc := cfg.Types["weight"]
	if tc.Columns["weight_kg"].Scale != 0.001 {
		t.Errorf("expected explicit scale to be kept, got %v", tc.Columns["weight_kg"].Scale)
	}
	if tc.Columns["note"].Scale != 1 {
		t.Errorf("expected unset scale to default to 1, got %v", tc.Columns["note"].Scale)
	}
}

func TestLoad_IncludeDurationAllowsEmptyColumns(t *testing.T) {
	path := writeConfig(t, `
types:
  sleep:
    source_table: sleep_session_record_table
    time_layout: interval
    columns: {}
    include_duration: true
    window: all
    daily: [sum, count]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names := cfg.Types["sleep"].ValueNames()
	if len(names) != 1 || names[0] != "duration_min" {
		t.Errorf("expected [duration_min], got %v", names)
	}
}

func TestLoad_Validation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"empty types", `types: {}`},
		{"invalid type key", `
types:
  BloodPressure:
    source_table: t
    time_layout: instant
    columns: {v: {column: c}}
    window: all
    daily: [mean]
`},
		{"invalid value name", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {Systolic: {column: c}}
    window: all
    daily: [mean]
`},
		{"reserved value name", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {duration_min: {column: c}}
    window: all
    daily: [mean]
`},
		{"empty source_table", `
types:
  bp:
    source_table: ""
    time_layout: instant
    columns: {v: {column: c}}
    window: all
    daily: [mean]
`},
		{"empty columns without include_duration", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {}
    window: all
    daily: [mean]
`},
		{"empty column name", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: ""}}
    window: all
    daily: [mean]
`},
		{"negative scale", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: c, scale: -1}}
    window: all
    daily: [mean]
`},
		{"invalid window", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: c}}
    window: "1w"
    daily: [mean]
`},
		{"invalid daily function", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: c}}
    window: all
    daily: [median]
`},
		{"duplicate daily function", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: c}}
    window: all
    daily: [mean, mean]
`},
		{"empty daily", `
types:
  bp:
    source_table: t
    time_layout: instant
    columns: {v: {column: c}}
    window: all
    daily: []
`},
		{"invalid time_layout", `
types:
  bp:
    source_table: t
    time_layout: bogus
    columns: {v: {column: c}}
    window: all
    daily: [mean]
`},
		{"series without series_table", `
types:
  hr:
    source_table: t
    time_layout: series
    columns: {v: {column: c}}
    window: all
    daily: [mean]
`},
		{"series_table set for instant", `
types:
  bp:
    source_table: t
    time_layout: instant
    series_table: s
    columns: {v: {column: c}}
    window: all
    daily: [mean]
`},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.yaml)
			if _, err := Load(path); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestTypeConfig_WindowDuration(t *testing.T) {
	cases := []struct {
		window        string
		wantDuration  time.Duration
		wantUnlimited bool
		wantErr       bool
	}{
		{"all", 0, true, false},
		{"30d", 30 * 24 * time.Hour, false, false},
		{"0d", 0, false, false},
		{"1w", 0, false, true},
		{"", 0, false, true},
	}
	for _, tt := range cases {
		t.Run(tt.window, func(t *testing.T) {
			tc := TypeConfig{Window: tt.window}
			d, unlimited, err := tc.WindowDuration()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d != tt.wantDuration || unlimited != tt.wantUnlimited {
				t.Errorf("got (%v, %v), want (%v, %v)", d, unlimited, tt.wantDuration, tt.wantUnlimited)
			}
		})
	}
}

func TestTypeConfig_ValueNames(t *testing.T) {
	tc := TypeConfig{
		Columns: map[string]ColumnConfig{
			"zeta":  {Column: "z"},
			"alpha": {Column: "a"},
		},
		IncludeDuration: true,
	}
	got := tc.ValueNames()
	want := []string{"alpha", "zeta", "duration_min"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConfig_TypeKeys(t *testing.T) {
	c := &Config{Types: map[string]TypeConfig{
		"zeta":  {},
		"alpha": {},
		"mid":   {},
	}}
	got := c.TypeKeys()
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
