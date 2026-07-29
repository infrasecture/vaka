package main

import (
	"fmt"
	"strings"
)

// PullPolicy controls whether vaka fetches a service image it must inspect
// (for ENTRYPOINT/USER defaults) when that image is not present locally.
//
// It governs *service* images only. vaka's own helper image
// (emsi/vaka-init:<version>) is always ensured via EnsureImage regardless of
// this policy — it is a trusted, vaka-managed image, not recipe content.
type PullPolicy int

const (
	// PullNever never fetches; a missing image is an error. This is the zero
	// value so a bare dockerServices{} keeps the original behavior.
	PullNever PullPolicy = iota
	// PullMissingPinned fetches a missing image only when it is pinned by
	// digest (@sha256:…) — content-addressed and verified, so safe to fetch
	// implicitly. This is the product default.
	PullMissingPinned
	// PullMissing fetches any missing image, pinned or not.
	PullMissing
)

// ParsePullPolicy maps the --vaka-pull flag value to a PullPolicy. The empty
// string selects the default (missing-pinned).
func ParsePullPolicy(s string) (PullPolicy, error) {
	switch strings.TrimSpace(s) {
	case "", "missing-pinned":
		return PullMissingPinned, nil
	case "missing":
		return PullMissing, nil
	case "never":
		return PullNever, nil
	default:
		return PullNever, fmt.Errorf("invalid --vaka-pull value %q: want one of missing-pinned|missing|never", s)
	}
}

func (p PullPolicy) String() string {
	switch p {
	case PullMissingPinned:
		return "missing-pinned"
	case PullMissing:
		return "missing"
	default:
		return "never"
	}
}

// pullsMissing reports whether a *missing* image with this ref should be
// fetched under the policy.
func (p PullPolicy) pullsMissing(ref string) bool {
	switch p {
	case PullMissing:
		return true
	case PullMissingPinned:
		return isDigestPinned(ref)
	default: // PullNever
		return false
	}
}

// isDigestPinned reports whether an image reference carries a `@sha256:<hex>`
// digest, i.e. is content-addressed and immutable. A tag (with or without a
// digest) is fine — only the digest presence matters.
func isDigestPinned(ref string) bool {
	i := strings.LastIndex(ref, "@sha256:")
	if i < 0 {
		return false
	}
	hex := ref[i+len("@sha256:"):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
