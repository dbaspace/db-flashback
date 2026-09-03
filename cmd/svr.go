package cmd

import (
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"db-flashback/internal/config"
	"db-flashback/internal/server"
	"db-flashback/pkg/signals"
	"db-flashback/pkg/utils/log"
)

var svrCmd = &cobra.Command{
	Use:   "svr",
	Short: "run flashback HTTP service",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := new(config.SvrConfig)
		if err := config.Init(cfg, readConfig()); err != nil {
			slog.Error("failed to init configuration", "error", err)
			os.Exit(1)
		}
		log.Logger().Sugar().Infow("configuration loaded", "port", cfg.HTTP.Port)

		srv, err := server.NewSvr(cfg)
		if err != nil {
			slog.Error("failed to init server", "error", err)
			os.Exit(1)
		}

		stopCh := signals.SetupSignalHandler()
		go func() {
			if err := srv.Run(); err != nil {
				slog.Error("failed to run server", "error", err)
				os.Exit(1)
			}
			os.Exit(0)
		}()

		sd, _ := signals.NewShutdown(5 * time.Second)
		sd.Graceful(stopCh, srv.Shutdown)
	},
}

func init() {
	rootCmd.AddCommand(svrCmd)
}
