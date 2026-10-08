package telegram

import (
	"net/url"
	"regexp"
	"strings"
)

var botTokenPattern = regexp.MustCompile(`[0-9]{6,}(?::|%3[Aa])[A-Za-z0-9_-]{20,}`)
var botEndpointPattern = regexp.MustCompile(`(?i)(https?://[^\s/]+/bot)[^/\s?"'<>]+`)

// RedactSecrets also handles older errors whose bot token may have been rotated.
func RedactSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	message = botTokenPattern.ReplaceAllString(message, "[REDACTED]")
	return botEndpointPattern.ReplaceAllString(message, "${1}[REDACTED]")
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

func redactError(err error, token string) error {
	if err == nil {
		return nil
	}
	message := RedactSecrets(err.Error(), token)
	if message == err.Error() {
		return err
	}
	return &redactedError{message: message, cause: err}
}
