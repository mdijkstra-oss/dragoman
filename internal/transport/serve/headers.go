package serve

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"strings"
)

// LogHeaderVar names the environment variable holding the request headers the
// accounting line carries. A caller in front of dragoman attaches its own
// identity — a request ID, a session, whichever agent it resolved — and copying
// those onto the record is what makes two processes' logs one story about one
// request.
const LogHeaderVar = "LOG_REQUEST_HEADERS"

// maxLoggedHeaders caps the allowlist. A record is a fixed set of fields an
// operator reads at a glance and a collector indexes; a request's headers are
// not, and an allowlist without a bound is a passthrough written the long way.
const maxLoggedHeaders = 16

// credentialTerms are the substrings a logged header may not contain. An
// operator who can name any header to log can name the one carrying the
// caller's key, and a credential written to a log is a credential in every
// archive of that log. The check is on the substring rather than the whole name
// because the spellings are endless and the intent is not.
var credentialTerms = []string{
	"access-token", "api-key", "auth", "credential", "password", "secret", "security-token",
}

// LoggedHeaders reads the allowlist a listener records under. It is an
// allowlist and never a passthrough: a request carrying fifty headers produces
// entries for the names configured here or none at all, and an unset variable
// is silence rather than whatever arrived.
//
// A value read this way is a label on a log line and nothing else. Dragoman
// performs no authentication, so a header naming a subject, a session or an
// agent grants nothing, reaches no routing decision, and selects no credential.
//
// It is read before the listener is built so that a name dragoman refuses to
// log is a refusal to start. Finding it out on the first request instead would
// be finding it out in production, with the record already missing.
func LoggedHeaders() ([]string, error) {
	value, configured := os.LookupEnv(LogHeaderVar)
	if !configured {
		return nil, nil
	}
	names := headerList(value)
	if err := checkHeaders(names); err != nil {
		return nil, err
	}
	return names, nil
}

// headerList reads the variable as the comma-separated list an operator writes.
// A name repeated in a different case is one name, kept in the spelling it was
// first given, so that a duplicate costs a slot in neither the cap nor the
// record.
func headerList(value string) []string {
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

// checkHeaders decides whether a table of names is one dragoman will record
// under. Every failure here names the entry that caused it: an operator who
// mistyped one header out of six should not have to bisect the list to find
// out which.
func checkHeaders(names []string) error {
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

// loggedHeaders reads the allowed names off one request. A name the request
// does not carry is left out rather than recorded empty, because an empty key
// claims a label arrived; a name repeated on the request is joined, because
// dropping either half would report something the caller did not send.
func loggedHeaders(header http.Header, allowed []string) []slog.Attr {
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
