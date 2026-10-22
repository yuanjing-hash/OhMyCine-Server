package quark

import (
	"context"
	"crypto/sha1"
	"encoding"
	"encoding/binary"
	"io"
	"testing"
)

func TestUploadPrefixSHA1IsLinearBeyondTwoGiB(t *testing.T) {
	const total int64 = 1<<31 + 64
	reader := &virtualSource{size: total}
	prefix := newUploadPrefixSHA1(reader)
	var words [5]uint32
	for offset := int64(4 << 20); offset < total; offset += 4 << 20 {
		if _, err := prefix.at(context.Background(), offset); err != nil {
			t.Fatal(err)
		}
		// Actual PUT consumption moves this same reader independently.
		_, _ = reader.Seek(total, io.SeekStart)
	}
	var err error
	words, err = prefix.at(context.Background(), total)
	if err != nil || reader.read != total {
		t.Fatalf("prefix rescanned: got %d want %d err=%v", reader.read, total, err)
	}
	if again, e := prefix.at(context.Background(), total); e != nil || again != words || reader.read != total {
		t.Fatal("same offset reread")
	}
}

func TestUploadPrefixSHA1MatchesColdResumeAndRejectsInvalidProgress(t *testing.T) {
	reader := &virtualSource{size: 4096}
	prefix := newUploadPrefixSHA1(reader)
	h := sha1.New()
	_, _ = io.CopyN(h, &virtualSource{size: 4096}, 2048)
	raw, _ := h.(encoding.BinaryMarshaler).MarshalBinary()
	var want [5]uint32
	for i := range want {
		want[i] = binary.BigEndian.Uint32(raw[4+i*4 : 8+i*4])
	}
	// Cold resume at a previously acknowledged boundary scans the prefix once.
	got, err := prefix.at(context.Background(), 2048)
	if err != nil || got != want || reader.read != 2048 {
		t.Fatal("wrong resumed digest")
	}
	for _, offset := range []int64{-1, 1024, 2049} {
		if _, e := prefix.at(context.Background(), offset); e == nil {
			t.Fatal("invalid progress")
		}
	}
	if _, e := prefix.at(context.Background(), 4096); e != nil || reader.read != 4096 {
		t.Fatal("incremental resume")
	}
	if _, e := prefix.at(context.Background(), 4160); e == nil {
		t.Fatal("short source")
	}
	if _, e := prefix.at(context.Background(), 4096); e == nil {
		t.Fatal("failed prefix reused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := newUploadPrefixSHA1(reader).at(ctx, 64); e == nil {
		t.Fatal("cancel ignored")
	}
}
