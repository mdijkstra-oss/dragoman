package serve

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"strings"
)

const LogHeaderVar = "LOG_REQUEST_HEADERS"

const maxLoggedHeaders = 16

// A credential written to a log is a credential in every archive of that log.
// Matched on the substring because the spellings are endless and the intent is
// not.
var credentialTerms = []string{
	"access-token", "api-key", "auth", "credential", "password", "secret", "security-token",
}

func Allowlist() ([]string, error) {
	value, configured := os.LookupEnv(LogHeaderVar)
	if !configured {
		return nil, nil
	}
	names := parseNames(value)
	if err := validateAllowlist(names); err != nil {
		return nil, err
	}
	return names, nil
}

// HTTP header names are case-insensitive, so a name repeated in a different case
// is one name and costs a slot in neither the cap nor the record.
func parseNames(value string) []string {
	names := make([]string, 0, strings.Count(value, ",")+1)
	seen := map[string]struct{}{}
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, repeated := seen[key]; repeated {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	return names
}

func validateAllowlist(names []string) error {
	if len(names) > maxLoggedHeaders {
		return fmt.Errorf("%s names %d headers; at most %d may be logged",
			LogHeaderVar, len(names), maxLoggedHeaders)
	}
	for _, name := range names {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if !strings.HasPrefix(canonical, "X-") {
			return fmt.Errorf("%s names %q; only X- headers may be logged", LogHeaderVar, name)
		}
		if isCredential(canonical) {
			return fmt.Errorf("%s names %q, which reads as a credential; a log line is no place for one",
				LogHeaderVar, name)
		}
	}
	return nil
}

func isCredential(canonical string) bool {
	normalized := strings.ToLower(canonical)
	for _, term := range credentialTerms {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return false
}

// An empty key would claim a label arrived, and dropping half of a repeated
// header would report something the caller did not send.
func headerAttrs(header http.Header, allowed []string) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(allowed))
	for _, name := range allowed {
		value := strings.Join(header.Values(name), ",")
		if value == "" {
			continue
		}
		attrs = append(attrs, slog.String(strings.ToLower(name), value))
	}
	return attrs
}
