package backends

import (
	"fmt"
	"strings"

	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
)

const aliasReplyAsHeader = "X-Guerrilla-Reply-As"

// ----------------------------------------------------------------------------------
// Processor Name: aliasresolve
// ----------------------------------------------------------------------------------
// Description   : Resolves reply-as alias from In-Reply-To / References via AliasStore
// ----------------------------------------------------------------------------------
// Config Options:
//
//	alias_db_path string - path to alias SQLite database
//	alias_fail_hard bool (omitempty) - reject SMTP on lookup miss (default true)
//	alias_index_ttl string (omitempty) - TTL for purge helper shared with alias-index
//	alias_index_max_rows int (omitempty) - max rows for purge helper
//
// ----------------------------------------------------------------------------------
func init() {
	processors["aliasresolve"] = func() Decorator {
		return AliasResolve()
	}
}

type aliasResolveConfig struct {
	DBPath   string
	FailHard bool
	StoreCfg AliasStoreConfig
}

func AliasResolve() Decorator {
	var cfg *aliasResolveConfig
	var store *AliasStore

	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		dbPath, _ := backendConfig["alias_db_path"].(string)
		dbPath = strings.TrimSpace(dbPath)
		if dbPath == "" {
			return fmt.Errorf("alias_db_path is required when using aliasresolve processor")
		}

		failHard := true
		if v, ok := backendConfig["alias_fail_hard"].(bool); ok {
			failHard = v
		}

		ttl := defaultAliasIndexTTL
		if raw, ok := backendConfig["alias_index_ttl"].(string); ok && strings.TrimSpace(raw) != "" {
			d, err := ParseAliasDuration(raw)
			if err != nil {
				return fmt.Errorf("invalid alias_index_ttl: %w", err)
			}
			if d > 0 {
				ttl = d
			}
		}

		maxRows := defaultAliasIndexMaxRows
		if v, ok := backendConfig["alias_index_max_rows"].(float64); ok && int(v) > 0 {
			maxRows = int(v)
		} else if v, ok := backendConfig["alias_index_max_rows"].(int); ok && v > 0 {
			maxRows = v
		}

		storeCfg := AliasStoreConfig{
			DBPath:  dbPath,
			TTL:     ttl,
			MaxRows: maxRows,
		}
		s, err := OpenAliasStore(storeCfg)
		if err != nil {
			return fmt.Errorf("open alias store: %w", err)
		}
		store = s
		cfg = &aliasResolveConfig{
			DBPath:   dbPath,
			FailHard: failHard,
			StoreCfg: storeCfg,
		}
		return nil
	}))

	Svc.AddShutdowner(ShutdownWith(func() error {
		if store != nil {
			return store.Close()
		}
		return nil
	}))

	return func(p Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
			if task != TaskSaveMail {
				return p.Process(e, task)
			}
			if cfg == nil || store == nil {
				err := fmt.Errorf("aliasresolve processor not initialized")
				if cfg != nil && !cfg.FailHard {
					Log().WithError(err).Warn("AliasResolve not initialized; skipping")
					return p.Process(e, task)
				}
				return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
			}

			if e.Header == nil {
				_ = e.ParseHeaders()
			}
			if !isReplyEnvelope(e) {
				return p.Process(e, task)
			}

			messageIDs := collectReplyMessageIDs(e)
			if len(messageIDs) == 0 {
				err := fmt.Errorf("reply message missing parseable In-Reply-To/References Message-ID")
				return aliasResolveFail(cfg.FailHard, e, messageIDs, "", err, p, task)
			}

			thread, matchedID, err := store.LookupAnyThread(messageIDs)
			if err != nil {
				return aliasResolveFail(cfg.FailHard, e, messageIDs, "", err, p, task)
			}
			if thread == nil {
				inReplyTo := headerFirst(e, "In-Reply-To")
				err := fmt.Errorf(
					"alias not found for In-Reply-To %s; cannot determine reply-as address (index expired or mail predates alias-index)",
					inReplyTo,
				)
				return aliasResolveFail(cfg.FailHard, e, messageIDs, matchedID, err, p, task)
			}

			if e.Values == nil {
				e.Values = make(map[string]interface{})
			}
			e.Values["alias_reply_as"] = thread.ReplyAs

			if len(e.RcptTo) > 0 {
				rcpt := strings.ToLower(e.RcptTo[0].String())
				if rcpt != "" && thread.OrigFrom != "" && rcpt != thread.OrigFrom {
					Log().WithFields(map[string]interface{}{
						"rcpt_to":    rcpt,
						"orig_from":  thread.OrigFrom,
						"message_id": matchedID,
					}).Warn("AliasResolve: RCPT TO does not match indexed orig_from; continuing")
				}
			}

			if e.Header == nil {
				e.Header = make(map[string][]string)
			}
			e.Header.Set(aliasReplyAsHeader, thread.ReplyAs)
			return p.Process(e, task)
		})
	}
}

func isReplyEnvelope(e *mail.Envelope) bool {
	if e.Header == nil {
		return false
	}
	return headerFirst(e, "In-Reply-To") != "" || headerFirst(e, "References") != ""
}

func collectReplyMessageIDs(e *mail.Envelope) []string {
	var ids []string
	seen := make(map[string]struct{})
	add := func(headerName string) {
		for _, id := range parseMessageIDs(headerFirst(e, headerName)) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	add("In-Reply-To")
	add("References")
	return ids
}

func headerFirst(e *mail.Envelope, name string) string {
	if e.Header == nil {
		return ""
	}
	return strings.TrimSpace(e.Header.Get(name))
}

func aliasResolveFail(
	failHard bool,
	e *mail.Envelope,
	searched []string,
	matchedID string,
	err error,
	p Processor,
	task SelectTask,
) (Result, error) {
	Log().WithError(err).WithFields(map[string]interface{}{
		"in_reply_to": headerFirst(e, "In-Reply-To"),
		"references":  headerFirst(e, "References"),
		"searched":    searched,
		"matched_id":  matchedID,
		"rcpt_to":     firstRcpt(e),
	}).Warn("AliasResolve lookup failed")
	if !failHard {
		return p.Process(e, task)
	}
	return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
}

func firstRcpt(e *mail.Envelope) string {
	if len(e.RcptTo) == 0 {
		return ""
	}
	return e.RcptTo[0].String()
}
