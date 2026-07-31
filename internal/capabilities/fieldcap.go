package capabilities

// Named per-field caps for every string admitted from a Kubernetes source, sharing the
// control plane's contract-boundary ceilings. All admitted source strings are untrusted;
// capping bounds what a hostile or misbehaving source can push into results.
//
// Deliberate, recorded compatibility deviation: the control plane slices UTF-16 code
// units; these caps count runes. Both agree on every BMP (and thus every ASCII) input —
// all real identifiers, reasons, and images — and diverge only for over-cap strings
// containing non-BMP characters, where rune capping is the safer behavior: it can never
// split a surrogate pair and emit a lone surrogate.
const (
	// MaxIdentifierChars is the DNS-1123 subdomain ceiling: names, namespaces, nodes, uids.
	MaxIdentifierChars = 253
	// MaxReasonChars bounds reason codes (OOMKilled, CrashLoopBackOff, FailedScheduling);
	// anything longer is hostile.
	MaxReasonChars = 128
	// MaxImageChars bounds image references (registry/repository:tag@digest).
	MaxImageChars = 256
	// MaxQuantityChars bounds resource quantity strings (e.g. 500m, 2Gi).
	MaxQuantityChars = 32
	// MaxMessageChars bounds the free text a cluster writes about itself — event messages
	// and nothing else. It matches the Kubernetes server-side event message ceiling, so a
	// well-behaved cluster is never truncated and a misbehaving one is.
	MaxMessageChars = 1024
	// MaxLogLineChars bounds one line of container output. A single line longer than this
	// is a payload rather than a log line, and the byte bound on the whole read is what
	// stops many of them; this stops one of them from being the whole read.
	MaxLogLineChars = 8192
)

func CapIdentifier(value string) string { return CapChars(value, MaxIdentifierChars) }

func CapReason(value string) string { return CapChars(value, MaxReasonChars) }

func CapImage(value string) string { return CapChars(value, MaxImageChars) }

func CapQuantity(value string) string { return CapChars(value, MaxQuantityChars) }

func CapMessage(value string) string { return CapChars(value, MaxMessageChars) }

func CapLogLine(value string) string { return CapChars(value, MaxLogLineChars) }

// CapChars truncates to at most maxChars runes. The byte-length fast path is not an
// optimisation detail: it is what makes the common case allocation-free, and every string
// that reaches it is one a source chose the length of.
func CapChars(value string, maxChars int) string {
	if len(value) <= maxChars {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	return string(runes[:maxChars])
}
