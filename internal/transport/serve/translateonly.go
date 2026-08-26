package serve

import (
	"os"
	"strings"
)

// TranslateOnlyVar names the variable that stops this instance short of the
// call. It answers every request with the body the backend would have received,
// which is what --no-execute writes to stdout. An instance in this mode reaches
// no provider at all, so it spends nothing and needs no credential.
//
// The mode is the instance, not the request: a listener either translates or
// serves, and one that does both would put a real call one forgotten parameter
// away.
const TranslateOnlyVar = "TRANSLATE_ONLY"

func TranslateOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(TranslateOnlyVar))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
