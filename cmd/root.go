package cmd

import (
	"log"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "db-flashback",
	Short: "standalone database flashback service",
	Long:  "parse PostgreSQL WAL / MySQL binlog and generate undo or redo SQL",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var cfg string

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfg, "config", "c", "configs/config.yaml", "config file")
	hostname, _ := os.Hostname()
	viper.Set("hostname", hostname)
}

func configFilePath() string { return cfg }

func readConfig() string {
	slog.Info("config file", "path", cfg)
	if _, err := os.Stat(cfg); err != nil {
		log.Fatalf("config file not found: %s", cfg)
	}
	content, err := os.ReadFile(cfg)
	if err != nil {
		log.Fatalf("error reading config file: %s", err.Error())
	}
	return string(content)
}
