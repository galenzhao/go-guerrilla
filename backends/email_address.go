package backends

import (
	"strings"

	"github.com/flashmob/go-guerrilla/mail"
)

// normalizeEmailAddress extracts a bare addr-spec from RFC5322 forms such as
// "Name <user@example.com>" or returns the input trimmed when already bare.
func normalizeEmailAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	a, err := mail.NewAddress(addr)
	if err != nil {
		// Best-effort fallback for malformed values.
		if i := strings.LastIndex(addr, "<"); i >= 0 {
			if j := strings.LastIndex(addr, ">"); j > i {
				return strings.ToLower(strings.TrimSpace(addr[i+1 : j]))
			}
		}
		return strings.ToLower(addr)
	}
	return strings.ToLower(a.String())
}

// extractFirstEmailFromHeader returns the first mailbox address from a header value.
func extractFirstEmailFromHeader(headerValue string) string {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return ""
	}
	a, err := mail.NewAddress(headerValue)
	if err == nil {
		return strings.ToLower(a.String())
	}
	// Multiple addresses: take the first comma-separated mailbox.
	for _, part := range splitAddresses(headerValue) {
		if normalized := normalizeEmailAddress(part); normalized != "" {
			return normalized
		}
	}
	return ""
}

// normalizeMessageID canonicalizes a Message-ID for index lookup.
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.Trim(id, "<>")
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if at := strings.LastIndex(id, "@"); at > 0 && at < len(id)-1 {
		local := id[:at]
		domain := strings.ToLower(id[at+1:])
		return local + "@" + domain
	}
	return strings.ToLower(id)
}

// parseMessageIDs extracts Message-IDs from In-Reply-To / References header values.
func parseMessageIDs(headerValue string) []string {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return nil
	}
	var ids []string
	seen := make(map[string]struct{})
	for _, token := range splitMessageIDTokens(headerValue) {
		norm := normalizeMessageID(token)
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		ids = append(ids, norm)
	}
	return ids
}

func splitMessageIDTokens(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var tokens []string
	var b strings.Builder
	inAngle := false
	for _, r := range s {
		switch r {
		case '<':
			if b.Len() > 0 {
				tokens = append(tokens, strings.TrimSpace(b.String()))
				b.Reset()
			}
			inAngle = true
		case '>':
			if inAngle {
				tokens = append(tokens, strings.TrimSpace(b.String()))
				b.Reset()
				inAngle = false
			} else {
				b.WriteRune(r)
			}
		default:
			if inAngle || r != ' ' && r != '\t' && r != '\r' && r != '\n' {
				b.WriteRune(r)
			} else if b.Len() > 0 {
				tokens = append(tokens, strings.TrimSpace(b.String()))
				b.Reset()
			}
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, strings.TrimSpace(b.String()))
	}
	return tokens
}
