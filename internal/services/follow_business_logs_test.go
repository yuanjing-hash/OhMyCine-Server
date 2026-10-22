package services

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type followProgressRuntime struct {
	providerWakeRuntime
	lastProgress             float64
	lastProcessed, lastTotal int64
}

func (r *followProgressRuntime) Heartbeat(progress *float64, processed, total *int64, _ *float64, _ *int64) error {
	if progress != nil {
		r.lastProgress = *progress
	}
	if processed != nil {
		r.lastProcessed = *processed
	}
	if total != nil {
		r.lastTotal = *total
	}
	return nil
}

func businessRecords(t *testing.T, buffer *bytes.Buffer, field, value string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid event: %v", err)
		}
		if record[field] == value {
			records = append(records, record)
		}
	}
	return records
}
