package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/terrpan/scaleset/internal/config"
	"github.com/terrpan/scaleset/internal/scaler"
)

var (
	cfgPath       string
	flagOverrides config.Config
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "scaleset",
	Short: "GitHub Actions Runner Scale Set -- compute-engine-agnostic runner autoscaler",
	Long: `scaleset registers a GitHub Actions Runner Scale Set and autoscales
ephemeral runners using a pluggable compute engine (Docker, EC2, etc.).

Configuration is read from a YAML file (--config) with optional CLI
flag overrides for the most common settings.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()
		return run(ctx)
	},
}

func init() {
	f := rootCmd.Flags()

	// Config file
	f.StringVar(&cfgPath, "config", "config.yaml", "Path to YAML configuration file")

	// GitHub overrides
	f.StringVar(&flagOverrides.GitHub.URL, "url", "", "GitHub URL for scale set registration (e.g. https://github.com/org/repo)")
	f.StringVar(&flagOverrides.GitHub.Token, "token", "", "Personal access token (alternative to GitHub App)")
	f.StringVar(&flagOverrides.GitHub.App.ClientID, "app-client-id", "", "GitHub App client ID")
	f.Int64Var(&flagOverrides.GitHub.App.InstallationID, "app-installation-id", 0, "GitHub App installation ID")
	f.StringVar(&flagOverrides.GitHub.App.PrivateKey, "app-private-key", "", "GitHub App private key (PEM)")
	f.StringVar(&flagOverrides.GitHub.App.PrivateKeyPath, "app-private-key-path", "", "Path to GitHub App private key PEM file")

	// Scale set overrides
	f.StringVar(&flagOverrides.ScaleSet.Name, "name", "", "Scale set name")
	f.IntVar(&flagOverrides.ScaleSet.MinRunners, "min-runners", 0, "Minimum number of runners")
	f.IntVar(&flagOverrides.ScaleSet.MaxRunners, "max-runners", 0, "Maximum number of runners")
	f.StringVar(&flagOverrides.ScaleSet.RunnerGroup, "runner-group", "", "Runner group name")

	// Logging overrides
	f.StringVar(&flagOverrides.Logging.Level, "log-level", "", "Log level (debug, info, warn, error)")
	f.StringVar(&flagOverrides.Logging.Format, "log-format", "", "Log format (text, json)")
}

// applyFlagOverrides merges non-zero CLI flag values into the loaded config.
func applyFlagOverrides(cfg *config.Config) {
	if flagOverrides.GitHub.URL != "" {
		cfg.GitHub.URL = flagOverrides.GitHub.URL
	}
	if flagOverrides.GitHub.Token != "" {
		cfg.GitHub.Token = flagOverrides.GitHub.Token
	}
	if flagOverrides.GitHub.App.ClientID != "" {
		cfg.GitHub.App.ClientID = flagOverrides.GitHub.App.ClientID
	}
	if flagOverrides.GitHub.App.InstallationID != 0 {
		cfg.GitHub.App.InstallationID = flagOverrides.GitHub.App.InstallationID
	}
	if flagOverrides.GitHub.App.PrivateKey != "" {
		cfg.GitHub.App.PrivateKey = flagOverrides.GitHub.App.PrivateKey
	}
	if flagOverrides.GitHub.App.PrivateKeyPath != "" {
		cfg.GitHub.App.PrivateKeyPath = flagOverrides.GitHub.App.PrivateKeyPath
	}
	if flagOverrides.ScaleSet.Name != "" {
		cfg.ScaleSet.Name = flagOverrides.ScaleSet.Name
	}
	if flagOverrides.ScaleSet.MinRunners != 0 {
		cfg.ScaleSet.MinRunners = flagOverrides.ScaleSet.MinRunners
	}
	if flagOverrides.ScaleSet.MaxRunners != 0 {
		cfg.ScaleSet.MaxRunners = flagOverrides.ScaleSet.MaxRunners
	}
	if flagOverrides.ScaleSet.RunnerGroup != "" {
		cfg.ScaleSet.RunnerGroup = flagOverrides.ScaleSet.RunnerGroup
	}
	if flagOverrides.Logging.Level != "" {
		cfg.Logging.Level = flagOverrides.Logging.Level
	}
	if flagOverrides.Logging.Format != "" {
		cfg.Logging.Format = flagOverrides.Logging.Format
	}
}

func run(ctx context.Context) error {
	// ---------------------------------------------------------------
	// 1. Load configuration
	// ---------------------------------------------------------------
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	applyFlagOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// ---------------------------------------------------------------
	// 2. Create logger
	// ---------------------------------------------------------------
	logger := cfg.NewLogger()
	logger.Info("configuration loaded",
		slog.String("configFile", cfgPath),
		slog.String("engine", cfg.Engine.Type),
		slog.String("scaleSetName", cfg.ScaleSet.Name),
		slog.Int("minRunners", cfg.ScaleSet.MinRunners),
		slog.Int("maxRunners", cfg.ScaleSet.MaxRunners),
	)

	// ---------------------------------------------------------------
	// 3. Create scaleset client
	// ---------------------------------------------------------------
	scalesetClient, err := cfg.NewScalesetClient()
	if err != nil {
		return fmt.Errorf("creating scaleset client: %w", err)
	}

	// ---------------------------------------------------------------
	// 4. Resolve runner group
	// ---------------------------------------------------------------
	var runnerGroupID int
	switch cfg.ScaleSet.RunnerGroup {
	case scaleset.DefaultRunnerGroup:
		runnerGroupID = 1
	default:
		rg, err := scalesetClient.GetRunnerGroupByName(ctx, cfg.ScaleSet.RunnerGroup)
		if err != nil {
			return fmt.Errorf("looking up runner group %q: %w", cfg.ScaleSet.RunnerGroup, err)
		}
		runnerGroupID = rg.ID
	}

	// ---------------------------------------------------------------
	// 5. Create runner scale set
	// ---------------------------------------------------------------
	scaleSet, err := scalesetClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          cfg.ScaleSet.Name,
		RunnerGroupID: runnerGroupID,
		Labels:        cfg.BuildLabels(),
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: true,
		},
	})
	if err != nil {
		return fmt.Errorf("creating runner scale set: %w", err)
	}

	logger.Info("runner scale set created",
		slog.Int("scaleSetID", scaleSet.ID),
		slog.String("name", scaleSet.Name),
	)

	scalesetClient.SetSystemInfo(scaleset.SystemInfo{
		System:     "terrpan-scaleset",
		Subsystem:  "cli",
		Version:    "0.1.0",
		CommitSHA:  "dev",
		ScaleSetID: scaleSet.ID,
	})

	defer func() {
		logger.Info("deleting runner scale set", slog.Int("scaleSetID", scaleSet.ID))
		if err := scalesetClient.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID); err != nil {
			logger.Error("failed to delete runner scale set",
				slog.Int("scaleSetID", scaleSet.ID),
				slog.String("error", err.Error()),
			)
		}
	}()

	// ---------------------------------------------------------------
	// 6. Initialize compute engine
	// ---------------------------------------------------------------
	eng, err := cfg.NewEngine(ctx, logger)
	if err != nil {
		return fmt.Errorf("initializing engine: %w", err)
	}

	// ---------------------------------------------------------------
	// 7. Create message session
	// ---------------------------------------------------------------
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid.NewString()
		logger.Warn("could not get hostname, using uuid",
			slog.String("fallback", hostname),
			slog.String("error", err.Error()),
		)
	}

	sessionClient, err := scalesetClient.MessageSessionClient(ctx, scaleSet.ID, hostname)
	if err != nil {
		return fmt.Errorf("creating message session: %w", err)
	}
	defer sessionClient.Close(context.Background())

	// ---------------------------------------------------------------
	// 8. Create listener + scaler
	// ---------------------------------------------------------------
	s := scaler.New(scaler.Config{
		ScaleSetID:     scaleSet.ID,
		MinRunners:     cfg.ScaleSet.MinRunners,
		MaxRunners:     cfg.ScaleSet.MaxRunners,
		ScalesetClient: scalesetClient,
		Engine:         eng,
		Logger:         logger.WithGroup("scaler"),
	})
	defer s.Shutdown(context.WithoutCancel(ctx))

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: cfg.ScaleSet.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}

	// ---------------------------------------------------------------
	// 9. Run
	// ---------------------------------------------------------------
	logger.Info("starting listener")
	if err := l.Run(ctx, s); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener: %w", err)
	}

	logger.Info("shutting down gracefully")
	return nil
}
