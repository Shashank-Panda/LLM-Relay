package catalog

import (
	"os"
	"strings"
)

// expandEnv substitutes ${VAR} and ${VAR:-default} from the environment.
//
// It is applied to exactly one field — an endpoint's base_url — and that
// restriction is the whole design. The catalog is data reviewed like data: a
// price in it has an attestation, a source URL, and a loader that refuses it
// once stale. A file whose *prices* could be rewritten by an environment
// variable would defeat every one of those controls at once, silently, and the
// savings report would go on quoting the substituted number with a straight
// face.
//
// A deployment address is not a price. Where Ollama listens differs between a
// laptop, a container and a CI runner, is not a claim about the world, and is
// the one thing that otherwise forces a second copy of the catalog to exist —
// and two catalogs are how two price tables drift apart.
//
// Deliberately not os.Expand: that treats an unset variable as empty, which
// here would turn a typo into a base_url of "" and silently fall back to the
// adapter's public host. Sending a prompt to api.openai.com because a variable
// name was misspelled is not a failure mode worth having. An unset variable
// with no default is left as written, so it fails the scheme check below with
// the literal text in the message.
func expandEnv(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}

	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			// Unterminated. Emit the rest verbatim so the validator reports the
			// text the author actually wrote.
			b.WriteString(s)
			return b.String()
		}
		j += i

		b.WriteString(s[:i])
		name, def, hasDef := strings.Cut(s[i+2:j], ":-")

		switch v, ok := os.LookupEnv(name); {
		case ok && v != "":
			b.WriteString(v)
		case hasDef:
			b.WriteString(def)
		default:
			b.WriteString(s[i : j+1])
		}
		s = s[j+1:]
	}
}
