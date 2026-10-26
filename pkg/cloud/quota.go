package cloud

import "context"

// QuotaReader is optional. Reading an account identity is not evidence that its
// authorization also exposes a trustworthy capacity measurement.
type QuotaReader interface {
	Quota(context.Context) (Quota, error)
}

type Quota struct {
	UsedBytes  uint64
	TotalBytes uint64
}
