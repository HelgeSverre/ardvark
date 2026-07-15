package main

import (
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Open the configured database and apply schema migrations",
	Long:  "migrate opens the configured database (storage.driver/storage.dsn) and runs GORM AutoMigrate for all ardvark tables, creating the schema if it does not already exist.",
	RunE:  runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	printer(cmd).Mutedf("migrated %s database at %s", cfg.Storage.Driver, cfg.Storage.DSN)
	return nil
}
