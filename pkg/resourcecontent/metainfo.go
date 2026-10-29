package resourcecontent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/anacrolix/torrent/merkle"
	"strconv"
)

// BuildMetainfo retains original metainfo when present. For verified v1 peer
// metadata it constructs only an outer dictionary, copying info bytes exactly.
// v2/hybrid metadata without original piece layers is deliberately not made
// executable by guessing a replacement container.
func BuildMetainfo(manifest Manifest) ([]byte, error) {
	if len(manifest.MetainfoBytes) > 0 {
		actual, err := ParseTorrent(manifest.MetainfoBytes, Limits{})
		if err != nil {
			return nil, err
		}
		if err = actual.ValidateIdentity(manifest.Identity); err != nil {
			return nil, err
		}
		if err = ValidateExecutableMetainfo(actual); err != nil {
			return nil, err
		}
		return bytes.Clone(manifest.MetainfoBytes), nil
	}
	if manifest.Identity.V2 != "" || manifest.Format != "v1" {
		return nil, invalid("original_v2_metainfo_required", ErrUnsupported)
	}
	actual, err := ParseInfo(manifest.InfoBytes, manifest.Identity, Limits{})
	if err != nil {
		return nil, err
	}
	if actual.Private && len(manifest.Trackers) == 0 {
		return nil, invalid("private_tracker_authority_missing", ErrUnsupported)
	}
	var buffer bytes.Buffer
	buffer.WriteByte('d')
	writeString := func(raw string) {
		buffer.WriteString(strconv.Itoa(len(raw)))
		buffer.WriteByte(':')
		buffer.WriteString(raw)
	}
	if len(manifest.Trackers) > 0 {
		count := 0
		writeString("announce-list")
		buffer.WriteByte('l')
		for _, tier := range manifest.Trackers {
			buffer.WriteByte('l')
			for _, tracker := range tier {
				count++
				if count > 64 {
					return nil, invalid("tracker_limit", ErrLimitExceeded)
				}
				if _, err = trackerURL(tracker); err != nil {
					return nil, err
				}
				writeString(tracker)
			}
			buffer.WriteByte('e')
		}
		buffer.WriteByte('e')
	}
	writeString("info")
	buffer.Write(manifest.InfoBytes)
	buffer.WriteByte('e')
	raw := buffer.Bytes()
	verified, err := ParseTorrent(raw, Limits{})
	if err != nil {
		return nil, err
	}
	if err = verified.ValidateIdentity(manifest.Identity); err != nil {
		return nil, err
	}
	return bytes.Clone(raw), nil
}

// ValidateExecutableMetainfo separates displayable info facts from complete
// v2 acquisition material. Required piece layers must exist, have the exact
// piece count and recompute to the original file tree's full Merkle root.
func ValidateExecutableMetainfo(manifest Manifest) error {
	if manifest.Identity.V2 == "" {
		return nil
	}
	if len(manifest.MetainfoBytes) == 0 {
		return invalid("original_v2_metainfo_required", ErrUnsupported)
	}
	if len(manifest.MetainfoBytes) > DefaultLimits().MaxMetainfoBytes || manifest.PieceLength < 16384 || manifest.PieceLength > 1<<30 || manifest.PieceLength&(manifest.PieceLength-1) != 0 {
		return invalid("executable_metainfo_invalid", ErrInvalidMetainfo)
	}
	root, err := decode(manifest.MetainfoBytes, normalizeLimits(Limits{}))
	if err != nil {
		return err
	}
	layers := root.get("piece layers")
	if layers != nil && layers.kind != 'd' {
		return invalid("piece_layers_invalid", ErrInvalidMetainfo)
	}
	var pad [32]byte
	for blocks := manifest.PieceLength / int64(merkle.BlockSize); blocks > 1; blocks /= 2 {
		pair := append(append([]byte(nil), pad[:]...), pad[:]...)
		pad = sha256.Sum256(pair)
	}
	for _, file := range manifest.Files {
		if file.Padding || file.Size <= manifest.PieceLength {
			continue
		}
		key, err := hex.DecodeString(file.PiecesRoot)
		if err != nil || len(key) != 32 {
			return invalid("pieces_root_invalid", ErrInvalidMetainfo)
		}
		if layers == nil || layers.get(string(key)) == nil {
			return invalid("v2_piece_layers_required", ErrUnsupported)
		}
		layer := layers.get(string(key))
		count := pieceCount(file.Size, manifest.PieceLength)
		if layer.kind != 's' || len(layer.data)%32 != 0 || int64(len(layer.data)/32) != count {
			return invalid("piece_layers_invalid", ErrInvalidMetainfo)
		}
		hashes, err := merkle.CompactLayerToSliceHashes(string(layer.data))
		if err != nil {
			return invalid("piece_layers_invalid", ErrInvalidMetainfo)
		}
		actual := merkle.RootWithPadHash(hashes, pad)
		if !bytes.Equal(actual[:], key) {
			return invalid("piece_layers_root_mismatch", ErrHashMismatch)
		}
	}
	return nil
}
