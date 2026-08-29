// health-connect-converter は Health Connect のエクスポートZIPを Drive から取得し、
// 累積SQLiteへ反映したうえで Google スプレッドシートへ書き出す常駐プロセス。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"health-connect-converter/internal/app"
	"health-connect-converter/internal/config"
	"health-connect-converter/internal/drivesource"
	"health-connect-converter/internal/hcreader"
	"health-connect-converter/internal/sheetssink"
	"health-connect-converter/internal/store"
)

func main() {
	once := flag.Bool("once", false, "1回だけ処理して終了する")
	flag.Parse()

	if err := run(context.Background(), *once, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type options struct {
	DriveFolderID string
	SpreadsheetID string
	SAKeyPath     string
	DBPath        string
	ConfigPath    string
	PollInterval  time.Duration
	LogLevel      string
}

func parseOptions(getenv func(string) string) (options, error) {
	opt := options{
		SAKeyPath:  envOrDefault(getenv, "HC_SA_KEY_PATH", "/run/secrets/sa-key.json"),
		DBPath:     envOrDefault(getenv, "HC_DB_PATH", "/data/health.db"),
		ConfigPath: envOrDefault(getenv, "HC_CONFIG_PATH", "/app/config.yaml"),
		LogLevel:   envOrDefault(getenv, "HC_LOG_LEVEL", "info"),
	}

	opt.DriveFolderID = getenv("HC_DRIVE_FOLDER_ID")
	opt.SpreadsheetID = getenv("HC_SPREADSHEET_ID")

	var missing []string
	if opt.DriveFolderID == "" {
		missing = append(missing, "HC_DRIVE_FOLDER_ID")
	}
	if opt.SpreadsheetID == "" {
		missing = append(missing, "HC_SPREADSHEET_ID")
	}
	if len(missing) > 0 {
		return options{}, fmt.Errorf("missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	intervalStr := envOrDefault(getenv, "HC_POLL_INTERVAL", "1h")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return options{}, fmt.Errorf("invalid HC_POLL_INTERVAL %q: %w", intervalStr, err)
	}
	if interval <= 0 {
		return options{}, fmt.Errorf("invalid HC_POLL_INTERVAL %q: must be positive", intervalStr)
	}
	opt.PollInterval = interval

	return opt, nil
}

func envOrDefault(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func run(ctx context.Context, once bool, getenv func(string) string) error {
	opt, err := parseOptions(getenv)
	if err != nil {
		return err
	}

	logLevel, knownLevel := knownLogLevels[opt.LogLevel]
	if !knownLevel {
		logLevel = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	if !knownLevel {
		logger.Warn("未知のHC_LOG_LEVEL。infoにフォールバックする", "value", opt.LogLevel)
	}

	cfg, err := config.Load(opt.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st, err := store.Open(opt.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx, cfg); err != nil {
		return fmt.Errorf("migrate store: %w", err)
	}

	src, err := drivesource.New(ctx, opt.SAKeyPath, opt.DriveFolderID)
	if err != nil {
		return fmt.Errorf("create drive source: %w", err)
	}

	sink, err := sheetssink.New(ctx, opt.SAKeyPath, opt.SpreadsheetID)
	if err != nil {
		return fmt.Errorf("create sheets sink: %w", err)
	}

	rd := &hcreader.Reader{TempDir: filepath.Join(filepath.Dir(opt.DBPath), "tmp")}

	a := app.New(cfg, src, rd, st, sink, logger, nil)

	logger.Info("起動",
		"config_path", opt.ConfigPath,
		"db_path", opt.DBPath,
		"poll_interval", opt.PollInterval,
		"once", once,
		"types", len(cfg.Types),
	)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if once {
		return a.RunOnce(ctx)
	}

	if err := a.Run(ctx, opt.PollInterval); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

var knownLogLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}
