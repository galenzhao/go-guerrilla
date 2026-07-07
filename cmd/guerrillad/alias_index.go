package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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

	backendConfig, err := loadBackendConfig(aliasIndexConfigPath)
	if err != nil {
		mainlog.WithError(err).Fatal("failed to load config")
	}

	indexerCfg, err := backends.AliasIndexerConfigFromBackend(backendConfig)
	if err != nil {
		mainlog.WithError(err).Fatal("invalid alias-index config")
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
	if len(indexerCfg.Accounts) > 0 {
		mailboxes := make([]string, 0, len(indexerCfg.Accounts))
		for _, account := range indexerCfg.Accounts {
			mailboxes = append(mailboxes, account.MailboxKey())
		}
		mainlog.WithField("mailboxes", mailboxes).Infof("alias-index watching %d POP3 mailbox(es)", len(mailboxes))
	}
	if err := indexer.Run(done); err != nil {
		mainlog.WithError(err).Fatal("alias-index stopped with error")
	}
	mainlog.Info("alias-index stopped")
}

func loadBackendConfig(path string) (backends.BackendConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var app struct {
		BackendConfig backends.BackendConfig `json:"backend_config"`
	}
	if err := json.Unmarshal(data, &app); err != nil {
		return nil, err
	}
	if app.BackendConfig == nil {
		return nil, fmt.Errorf("backend_config missing in %s", path)
	}
	return app.BackendConfig, nil
}
