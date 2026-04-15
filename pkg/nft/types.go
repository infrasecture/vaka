// pkg/nft/types.go
package nft

// RulesetData is the data passed to egress.nft.tmpl.
// All rule strings are pre-rendered by the generator; the template only
// formats them.
type RulesetData struct {
	BlockMetadata  bool
	MetadataRanges []string // e.g. "ip  daddr 169.254.0.0/16"
	DropRules      []string
	RejectRules    []string
	AcceptRules    []string
	DefaultVerdict string // e.g. "reject with icmpx type port-unreachable"
}
