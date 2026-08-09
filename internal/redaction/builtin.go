package redaction

import "fmt"

// builtinPrefix marks the rules a customer policy may add to and can never disable. A
// customer identifier carrying it is refused at parse time, so no policy can impersonate a
// default in order to replace it.
const builtinPrefix = "builtin."

// The built-in default set. It is on from the first install with no configuration, and it
// covers high-confidence shapes ONLY.
//
// The bar for a default is that a false positive is nearly impossible. Anything with a
// meaningful false-positive rate belongs in customer-authored policy, because a default that
// destroys evidence is worse than no default: an investigation that loses the decisive log
// line to an over-eager pattern fails in a way nobody can see, and the product is blamed for
// the hole rather than the rule.
//
// Every entry here matches a credential SHAPE — a delimiter and an encoding — rather than a
// value that merely looks secret. Statuses, reasons, identifiers and quantities are the
// substance of an investigation and no rule here may claim them.
var builtinRules = []struct{ id, pattern string }{
	{
		// A PEM private key block, from its opening armour to its closing armour. Non-greedy so
		// two keys in one field are two spans rather than one span swallowing what lies between.
		id:      builtinPrefix + "private_key_block",
		pattern: `(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`,
	},
	{
		// An Authorization header and its value, however the surrounding format quotes it. The
		// scheme is masked along with the credential: "Bearer" on its own tells a reader nothing
		// the rule identifier does not.
		id:      builtinPrefix + "authorization_header",
		pattern: `(?i)\bauthorization["']?\s*[:=]\s*["']?(?:bearer|basic|digest|token)?\s*[A-Za-z0-9\-._~+/]+={0,2}`,
	},
	{
		// A bearer credential presented without the header around it, as client libraries log it.
		id:      builtinPrefix + "bearer_token",
		pattern: `(?i)\bbearer\s+[A-Za-z0-9\-._~+/]{16,}={0,2}`,
	},
	{
		// A JSON Web Token: three base64url segments, the first of which begins the encoded
		// "{"alg":" header every JWT starts with. The prefix is what makes this high-confidence
		// rather than a rule about any three dotted words.
		id:      builtinPrefix + "json_web_token",
		pattern: `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	},
	{
		// A cloud provider access key identifier. The prefixes are assigned and the length is
		// fixed, so a false positive would have to be a deliberate imitation.
		id:      builtinPrefix + "cloud_access_key_id",
		pattern: `\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ABIA)[0-9A-Z]{16}\b`,
	},
	{
		// The secret half of the same credential, which has no distinguishing shape of its own
		// and is therefore recognised by the name it is assigned to.
		id:      builtinPrefix + "cloud_secret_access_key",
		pattern: `(?i)\baws_?secret_?access_?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{20,}`,
	},
	{
		// A connection string carrying credentials: scheme://user:password@host. Only the
		// credential-bearing form matches — a plain scheme://host is a destination, which is
		// evidence, and masking it would destroy the answer to "what could it not reach".
		id:      builtinPrefix + "credentialed_connection_string",
		pattern: `(?i)\b[a-z][a-z0-9+.\-]*://[^\s:/@]+:[^\s/@]+@`,
	},
	{
		// A credential assigned to a name that says what it is. The name list is closed and every
		// entry on it names a secret rather than a setting, which is what keeps this inside the
		// false-positive bar: "status=refused" and "retry=3" are untouched.
		id: builtinPrefix + "credential_assignment",
		pattern: `(?i)\b(?:password|passwd|pwd|secret|api[_-]?key|apikey|access[_-]?token|` +
			`auth[_-]?token|client[_-]?secret|private[_-]?key|token)["']?\s*[:=]\s*["']?[^\s"',;]{4,}`,
	},
}

// BuiltIn compiles the default rules. It panics on a malformed pattern because these are
// compiled-in constants: a broken default is a defect in this file that every install would
// carry, and a Relay that started anyway would be one whose masking silently did less than it
// claims.
func BuiltIn() []Rule {
	rules := make([]Rule, 0, len(builtinRules))
	for _, declared := range builtinRules {
		rule, err := NewRule(declared.id, declared.pattern)
		if err != nil {
			panic(fmt.Sprintf("built-in redaction rule %s does not compile: %v", declared.id, err))
		}
		rules = append(rules, rule)
	}
	return rules
}
