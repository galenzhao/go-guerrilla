package backends

import (
	"time"
)

type indexMessageOpts struct {
	mailboxKey  string
	mailboxUser string
	tenantID    string
	since       time.Time
	folder      string
}

type indexMessageOutcome int

const (
	outcomeIndexed indexMessageOutcome = iota
	outcomeSkippedSeen
	outcomeSkippedDate
	outcomeNotIndexable
)

func indexMessageFromHeaders(store *AliasStore, headers parsedMailHeaders, opts indexMessageOpts) (indexMessageOutcome, error) {
	messageID := normalizeMessageID(headers.MessageID)
	if messageID != "" {
		seen, err := store.IsSeenMessage(opts.mailboxKey, messageID)
		if err != nil {
			return outcomeNotIndexable, err
		}
		if seen {
			return outcomeSkippedSeen, nil
		}
	}

	if !opts.since.IsZero() && !headers.Date.IsZero() && headers.Date.Before(opts.since) {
		if messageID != "" {
			if err := store.MarkSeenMessage(opts.mailboxKey, messageID); err != nil {
				return outcomeNotIndexable, err
			}
		}
		return outcomeSkippedDate, nil
	}

	replyAs := extractReplyAsAddress(headers, opts.mailboxUser)
	origFrom := extractFirstEmailFromHeader(headers.From)
	if messageID == "" || replyAs == "" || origFrom == "" {
		fields := map[string]interface{}{
			"mailbox":    opts.mailboxKey,
			"message_id": headers.MessageID,
			"to":         headers.To,
			"delivered":  headers.DeliveredTo,
			"x_orig":     headers.XOriginalTo,
			"from":       headers.From,
		}
		if opts.folder != "" {
			fields["folder"] = opts.folder
		}
		Log().WithFields(fields).Debug("alias-index skipping message without indexable headers")
		if messageID != "" {
			if err := store.MarkSeenMessage(opts.mailboxKey, messageID); err != nil {
				return outcomeNotIndexable, err
			}
		}
		return outcomeNotIndexable, nil
	}

	if err := store.UpsertThread(AliasThread{
		MessageID: messageID,
		ReplyAs:   replyAs,
		OrigFrom:  origFrom,
		TenantID:  opts.tenantID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return outcomeNotIndexable, err
	}
	if err := store.MarkSeenMessage(opts.mailboxKey, messageID); err != nil {
		return outcomeNotIndexable, err
	}
	return outcomeIndexed, nil
}
