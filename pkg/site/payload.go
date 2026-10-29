package site

// PayloadKind is safe display metadata about the source material an adapter
// already identified. It never grants acquisition permission or public provenance.
type PayloadKind string

const (
	PayloadUnknown PayloadKind = "unknown"
	PayloadTorrent PayloadKind = "torrent"
	PayloadMagnet  PayloadKind = "magnet"
	PayloadShare   PayloadKind = "share"
)

// NormalizePayloadKind keeps unknown/legacy adapter output explicit. Callers
// must not infer a private source URI or fetch it merely to populate this field.
func NormalizePayloadKind(kind PayloadKind) PayloadKind {
	switch kind {
	case PayloadTorrent, PayloadMagnet, PayloadShare:
		return kind
	default:
		return PayloadUnknown
	}
}
