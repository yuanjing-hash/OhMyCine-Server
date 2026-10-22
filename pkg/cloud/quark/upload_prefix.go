package quark

import (
	"context"
	"crypto/sha1"
	"encoding"
	"encoding/binary"
	"hash"
	"io"
)

// This cursor lives only within one upload invocation and is bound to that
// invocation's already whole-file-verified source. A resumed operation rebuilds
// its prefix once; it never trusts serialized hash state from a previous source.
// PUTs may move the same seekable reader, so every increment seeks to our own
// last hashed offset instead of assuming the reader's current position.
type uploadPrefixSHA1 struct {
	reader io.ReadSeeker
	hash   hash.Hash
	offset int64
	failed bool
}

func newUploadPrefixSHA1(reader io.ReadSeeker) *uploadPrefixSHA1 {
	return &uploadPrefixSHA1{reader: reader, hash: sha1.New()}
}

func (p *uploadPrefixSHA1) at(ctx context.Context, offset int64) ([5]uint32, error) {
	var words [5]uint32
	if p == nil || p.reader == nil || p.failed || offset < p.offset || offset < 0 || offset%sha1.BlockSize != 0 {
		return words, failure(CodeInvalid)
	}
	if err := ctx.Err(); err != nil {
		return words, err
	}
	if offset > p.offset {
		position, err := p.reader.Seek(p.offset, io.SeekStart)
		if err != nil || position != p.offset {
			p.failed = true
			return words, failure(CodeInvalid)
		}
		n, err := io.CopyN(p.hash, contextReader{ctx, p.reader}, offset-p.offset)
		p.offset += n
		if err != nil {
			p.failed = true
			if ctx.Err() != nil {
				return words, ctx.Err()
			}
			return words, failure(CodeInvalid)
		}
	}
	raw, err := p.hash.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil || len(raw) < 24 || string(raw[:4]) != "sha\x01" {
		return words, failure(CodeInvalid)
	}
	for i := range words {
		words[i] = binary.BigEndian.Uint32(raw[4+i*4 : 8+i*4])
	}
	return words, nil
}
