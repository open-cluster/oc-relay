package runtime

// Named per-field caps for every string admitted from a Kubernetes source, sharing the
// central projection's contract-boundary ceilings. All admitted source strings are
// untrusted; capping bounds what a hostile or misbehaving source can push into results.
// Free-text Kubernetes message fields are structurally absent from the projection and
// deliberately have no cap here.
//
// Deliberate, recorded deviation from the central implementation: it slices UTF-16
// code units; this cap counts runes. Both agree on every BMP (and thus every ASCII)
// input — all real identifiers, reasons, and images — and diverge only for over-cap
// strings containing non-BMP characters, where rune capping is the safer behavior: it
// can never split a surrogate pair and emit a lone surrogate.
const (
	// maxIdentifierChars is the DNS-1123 subdomain ceiling: names, namespaces, nodes, uids.
	maxIdentifierChars = 253
	// maxReasonChars bounds reason codes (OOMKilled, CrashLoopBackOff, ...); anything
	// longer is hostile.
	maxReasonChars = 128
	// maxImageChars bounds image references (registry/repository:tag@digest).
	maxImageChars = 256
)

func capIdentifier(value string) string { return capChars(value, maxIdentifierChars) }

func capReason(value string) string { return capChars(value, maxReasonChars) }

func capImage(value string) string { return capChars(value, maxImageChars) }

func capChars(value string, maxChars int) string {
	if len(value) <= maxChars {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	return string(runes[:maxChars])
}
