package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/flashmob/go-guerrilla"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/log"
	"github.com/spf13/cobra"
)

var aliasIndexConfigPath string

var aliasIndexCmd = &cobra.Command{
	Use:   "alias-index",
	Short: "poll POP3 mailbox and index Message-ID to reply-as alias mappings",
	Run:   runAliasIndex,
}

func init() {
	cfgFile := "goguerrilla.conf"
	if _, err := os.Stat(cfgFile); err != nil {
		cfgFile = "goguerrilla.conf.json"
	}
	aliasIndexCmd.Flags().StringVarP(&aliasIndexConfigPath, "config", "c", cfgFile, "Path to the configuration file")
	rootCmd.AddCommand(aliasIndexCmd)
}

func runAliasIndex(cmd *cobra.Command, args []string) {
	mainlog, err := log.GetLogger(log.OutputStderr.String(), log.InfoLevel.String())
	if err != nil && mainlog != nil {
		mainlog.WithError(err).Error("failed creating logger")
	}
	if verbose {
		mainlog, _ = log.GetLogger(log.OutputStderr.String(), log.DebugLevel.String())
	}

	appConfig, backendConfig, err := loadAppAndBackendConfig(aliasIndexConfigPath)
	if err != nil {
		mainlog.WithError(err).Fatal("failed to load config")
	}

	indexerCfg, err := backends.AliasIndexerConfigFromBackend(backendConfig)
	if err != nil {
		mainlog.WithError(err).Fatal("invalid alias-index config")
	}

	if appConfig.TenantRegistry.URL != "" {
		registry, err := guerrilla.NewTenantRegistryFromConfig(appConfig.TenantRegistry)
		if err != nil {
			mainlog.WithError(err).Fatal("failed to initialize tenant registry")
		}
		indexerCfg.Registry = registry
		if len(indexerCfg.Accounts) > 0 {
			mainlog.Warn("tenant_registry is configured; ignoring static alias_index_pop3_accounts")
			indexerCfg.Accounts = nil
		}
	} else if len(indexerCfg.Accounts) == 0 {
		mainlog.Fatal("either tenant_registry.url or alias_index_pop3_accounts is required")
	}

	indexer, err := backends.NewAliasIndexer(indexerCfg)
	if err != nil {
		mainlog.WithError(err).Fatal("failed to start alias-index")
	}
	defer indexer.Close()

	done := make(chan struct{})
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-signalCh
		close(done)
	}()

	mainlog.Info("alias-index started")
	accounts := indexerCfg.Accounts
	if indexerCfg.Registry != nil {
		if err := indexerCfg.Registry.Refresh(context.Background()); err != nil {
			mainlog.WithError(err).Warn("initial tenant registry refresh failed")
		} else {
			accounts = nil
			for _, item := range indexerCfg.Registry.POP3Accounts() {
				accounts = append(accounts, item.Account)
			}
		}
	}
	if len(accounts) > 0 {
		mailboxes := make([]string, 0, len(accounts))
		for _, account := range accounts {
			mailboxes = append(mailboxes, account.MailboxKey())
		}
		mainlog.WithField("mailboxes", mailboxes).Infof("alias-index watching %d POP3 mailbox(es)", len(mailboxes))
	}
	if err := indexer.Run(done); err != nil {
		mainlog.WithError(err).Fatal("alias-index stopped with error")
	}
	mainlog.Info("alias-index stopped")
}

func loadAppAndBackendConfig(path string) (guerrilla.AppConfig, backends.BackendConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return guerrilla.AppConfig{}, nil, err
	}
	var app guerrilla.AppConfig
	if err := json.Unmarshal(data, &app); err != nil {
		return guerrilla.AppConfig{}, nil, err
	}
	if app.BackendConfig == nil {
		return app, nil, fmt.Errorf("backend_config missing in %s", path)
	}
	return app, app.BackendConfig, nil
}

func loadBackendConfig(path string) (backends.BackendConfig, error) {
	_, backendConfig, err := loadAppAndBackendConfig(path)
	return backendConfig, err
}
