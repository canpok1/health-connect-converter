package main

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestParseOptions_Defaults(t *testing.T) {
	opt, err := parseOptions(envMap(map[string]string{
		"HC_DRIVE_FOLDER_ID": "folder1",
		"HC_SPREADSHEET_ID":  "sheet1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt.SAKeyPath != "/run/secrets/sa-key.json" {
		t.Errorf("SAKeyPath = %q", opt.SAKeyPath)
	}
	if opt.DBPath != "/data/health.db" {
		t.Errorf("DBPath = %q", opt.DBPath)
	}
	if opt.ConfigPath != "/app/config.yaml" {
		t.Errorf("ConfigPath = %q", opt.ConfigPath)
	}
	if opt.LogLevel != "info" {
		t.Errorf("LogLevel = %q", opt.LogLevel)
	}
	if opt.PollInterval != time.Hour {
		t.Errorf("PollInterval = %v", opt.PollInterval)
	}
}

func TestParseOptions_MissingFolderID(t *testing.T) {
	_, err := parseOptions(envMap(map[string]string{
		"HC_SPREADSHEET_ID": "sheet1",
	}))
	if err == nil || !strings.Contains(err.Error(), "HC_DRIVE_FOLDER_ID") {
		t.Fatalf("expected error mentioning HC_DRIVE_FOLDER_ID, got %v", err)
	}
}

func TestParseOptions_MissingBoth(t *testing.T) {
	_, err := parseOptions(envMap(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HC_DRIVE_FOLDER_ID") || !strings.Contains(err.Error(), "HC_SPREADSHEET_ID") {
		t.Fatalf("expected error mentioning both variables, got %v", err)
	}
}

func TestParseOptions_PollInterval(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"90m", 90 * time.Minute, false},
		{"abc", 0, true},
		{"0s", 0, true},
		{"-1h", 0, true},
	}
	for _, tt := range cases {
		t.Run(tt.value, func(t *testing.T) {
			opt, err := parseOptions(envMap(map[string]string{
				"HC_DRIVE_FOLDER_ID": "f",
				"HC_SPREADSHEET_ID":  "s",
				"HC_POLL_INTERVAL":   tt.value,
			}))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opt.PollInterval != tt.want {
				t.Errorf("PollInterval = %v, want %v", opt.PollInterval, tt.want)
			}
		})
	}
}
