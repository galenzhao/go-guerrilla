package backends

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
)

// ----------------------------------------------------------------------------------
// Processor Name: rewritefrom
// ----------------------------------------------------------------------------------
// Description   : Rewrites the RFC5322 "From:" header in the raw message, typically
//
//	to match an AWS SES verified identity.
//
// ----------------------------------------------------------------------------------
// Config Options:
//
//	ses_source_tmpl string - template for From address, same as SES/OCIEmail source template
//	ociemail_source_tmpl string (omitempty) - alternative to ses_source_tmpl for OCIEmail pipelines
//
// ----------------------------------------------------------------------------------
func init() {
	processors["rewritefrom"] = func() Decorator {
		return RewriteFrom()
	}
}

type rewriteFromConfig struct {
	SourceTmpl         string `json:"ses_source_tmpl,omitempty"`
	OCIEmailSourceTmpl string `json:"ociemail_source_tmpl,omitempty"`
}

func RewriteFrom() Decorator {
	var cfg *rewriteFromConfig

	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		configType := BaseConfig(&rewriteFromConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}
		cfg = bcfg.(*rewriteFromConfig)
		sourceTmpl := strings.TrimSpace(cfg.SourceTmpl)
		if sourceTmpl == "" {
			sourceTmpl = strings.TrimSpace(cfg.OCIEmailSourceTmpl)
		}
		if sourceTmpl == "" {
			return fmt.Errorf("ses_source_tmpl or ociemail_source_tmpl is required")
		}
		cfg.SourceTmpl = sourceTmpl
		return nil
	}))

	return func(p Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
			if task != TaskSaveMail {
				return p.Process(e, task)
			}
			if cfg == nil {
				err := fmt.Errorf("rewritefrom processor not initialized")
				return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
			}

			vars := makeSESVars(e)
			newFrom := strings.TrimSpace(applySESTemplate(cfg.SourceTmpl, vars))
			newFrom = normalizeEmailAddress(newFrom)
			if newFrom == "" {
				err := fmt.Errorf("ses template produced empty from")
				return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
			}

			rewritten, changed, rerr := rewriteFromHeader(e.Data.Bytes(), newFrom)
			if rerr != nil {
				return NewResult(response.Canned.FailBackendTransaction, response.SP, rerr), rerr
			}
			if changed {
				e.Data.Reset()
				_, _ = e.Data.Write(rewritten)
				// headers are now stale; reparse so downstream processors (eg Debugger) can inspect them
				preserved := ""
				if e.Header != nil {
					preserved = strings.TrimSpace(e.Header.Get(aliasReplyAsHeader))
				}
				e.Header = nil
				e.Subject = ""
				if err := e.ParseHeaders(); err != nil {
					// best-effort: don't fail the transaction if header parsing fails
					Log().WithError(err).Warn("rewritefrom: header parse failed after rewrite")
				}
				if preserved != "" {
					if e.Header == nil {
						e.Header = make(map[string][]string)
					}
					e.Header.Set(aliasReplyAsHeader, preserved)
				}
			}

			return p.Process(e, task)
		})
	}
}

func rewriteFromHeader(raw []byte, newFrom string) ([]byte, bool, error) {
	// Find header/body split (support \n\n and \r\n\r\n).
	var (
		splitIdx int
	)
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		splitIdx = i
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		splitIdx = i
	} else {
		return nil, false, fmt.Errorf("header not found")
	}

	header := raw[:splitIdx]
	body := raw[splitIdx:]

	newline := []byte("\n")
	if bytes.Contains(header, []byte("\r\n")) {
		newline = []byte("\r\n")
	}

	lines := bytes.Split(header, newline)
	out := make([][]byte, 0, len(lines)+1)

	found := false
	changed := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		lower := bytes.ToLower(line)
		if bytes.HasPrefix(lower, []byte("from:")) {
			found = true
			// Skip continuation lines.
			j := i + 1
			for j < len(lines) && len(lines[j]) > 0 && (lines[j][0] == ' ' || lines[j][0] == '\t') {
				j++
			}

			repl := []byte("From: " + newFrom)
			out = append(out, repl)
			if !bytes.Equal(line, repl) {
				changed = true
			}

			i = j
			continue
		}
		out = append(out, line)
		i++
	}

	if !found {
		out = append([][]byte{[]byte("From: " + newFrom)}, out...)
		changed = true
	}

	rebuiltHeader := bytes.Join(out, newline)
	rebuilt := append(append([]byte(nil), rebuiltHeader...), body...)
	return rebuilt, changed, nil
}
