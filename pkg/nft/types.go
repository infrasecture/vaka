// pkg/nft/types.go
package nft

// RulesetData is the data passed to egress.nft.tmpl.
// All rule strings are pre-rendered by the generator; the template only
// formats them.
type RulesetData struct {
	BlockMetadata       bool
	MetadataRanges      []string // e.g. "ip  daddr 169.254.169.254/32"
	DropRules           []string
	RejectRules         []string
	AcceptRules         []string
	DefaultVerdictLines []string // 1 or 2 terminal verdict statements
}
