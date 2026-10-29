package resourcecontent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	metadataPieceSize = 16384
	maxPeerFrame      = 64 << 10
	maxPeerMessages   = 2048
	localMetadataID   = 1
)

// fetchMetadata implements only the extension handshake and ut_metadata. No
// media-piece request or payload storage path exists in this protocol reader.
func fetchMetadata(ctx context.Context, network Network, address string, identity Identity, limits Limits) (Manifest, error) {
	short, err := identity.ShortHash()
	if err != nil {
		return Manifest{}, err
	}
	conn, err := network.DialContext(ctx, "tcp", address)
	if err != nil {
		return Manifest{}, invalid("peer_dial_failed", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var handshake [68]byte
	handshake[0] = 19
	copy(handshake[1:20], "BitTorrent protocol")
	handshake[25] = 0x10
	copy(handshake[28:48], short[:])
	copy(handshake[48:], "-OMC100-")
	if _, err = rand.Read(handshake[56:]); err != nil {
		return Manifest{}, invalid("peer_random_failed", err)
	}
	if err = writeAll(conn, handshake[:]); err != nil {
		return Manifest{}, invalid("peer_handshake_failed", err)
	}
	var remote [68]byte
	if _, err = io.ReadFull(conn, remote[:]); err != nil || remote[0] != 19 || string(remote[1:20]) != "BitTorrent protocol" || !bytes.Equal(remote[28:48], short[:]) || remote[25]&0x10 == 0 {
		return Manifest{}, invalid("peer_handshake_invalid", ErrInvalidMetainfo)
	}
	if err = writeExtension(conn, 0, []byte("d1:md11:ut_metadatai1eee")); err != nil {
		return Manifest{}, invalid("peer_handshake_failed", err)
	}
	remoteID, size := 0, 0
	messageCount := 0
	for remoteID == 0 {
		messageCount++
		if messageCount > maxPeerMessages {
			return Manifest{}, invalid("peer_message_limit", ErrLimitExceeded)
		}
		frame, e := readFrame(conn)
		if e != nil {
			return Manifest{}, e
		}
		if len(frame) < 2 || frame[0] != 20 || frame[1] != 0 {
			continue
		}
		header, e := decode(frame[2:], limits)
		if e != nil || header.kind != 'd' {
			return Manifest{}, invalid("extension_handshake_invalid", ErrInvalidMetainfo)
		}
		protocols, total := header.get("m"), header.get("metadata_size")
		if protocols == nil || protocols.kind != 'd' || protocols.get("ut_metadata") == nil || total == nil || total.kind != 'i' {
			return Manifest{}, invalid("metadata_extension_unavailable", ErrUnavailable)
		}
		id := protocols.get("ut_metadata")
		if id.kind != 'i' || id.n <= 0 || id.n > 255 || total.n <= 0 || total.n > int64(limits.MaxInfoBytes) {
			return Manifest{}, invalid("metadata_extension_limit", ErrLimitExceeded)
		}
		remoteID, size = int(id.n), int(total.n)
	}
	metadata := make([]byte, size)
	count := (size + metadataPieceSize - 1) / metadataPieceSize
	requested, received := make([]bool, count), make([]bool, count)
	next, complete := 0, 0
	requestMore := func() error {
		for next < count && next-complete < 4 {
			payload := []byte(fmt.Sprintf("d8:msg_typei0e5:piecei%dee", next))
			if err := writeExtension(conn, byte(remoteID), payload); err != nil {
				return err
			}
			requested[next] = true
			next++
		}
		return nil
	}
	if err = requestMore(); err != nil {
		return Manifest{}, invalid("metadata_request_failed", err)
	}
	for complete < count {
		messageCount++
		if messageCount > maxPeerMessages {
			return Manifest{}, invalid("peer_message_limit", ErrLimitExceeded)
		}
		frame, e := readFrame(conn)
		if e != nil {
			return Manifest{}, e
		}
		if len(frame) < 2 || frame[0] != 20 || frame[1] != localMetadataID {
			continue
		}
		d := decoder{raw: frame[2:], limits: limits}
		header, e := d.read(0)
		if e != nil || header.kind != 'd' || header.get("msg_type") == nil || header.get("piece") == nil {
			return Manifest{}, invalid("metadata_message_invalid", ErrInvalidMetainfo)
		}
		kind, index := header.get("msg_type"), header.get("piece")
		if kind.kind != 'i' || index.kind != 'i' || index.n < 0 || index.n >= int64(count) {
			return Manifest{}, invalid("metadata_piece_invalid", ErrInvalidMetainfo)
		}
		if kind.n == 2 {
			return Manifest{}, invalid("metadata_rejected", ErrUnavailable)
		}
		if kind.n != 1 {
			continue
		}
		i := int(index.n)
		total := header.get("total_size")
		data := frame[2+d.pos:]
		expected := metadataPieceSize
		if i == count-1 {
			expected = size - i*metadataPieceSize
		}
		if !requested[i] || received[i] || total == nil || total.kind != 'i' || total.n != int64(size) || len(data) != expected {
			return Manifest{}, invalid("metadata_piece_invalid", ErrInvalidMetainfo)
		}
		copy(metadata[i*metadataPieceSize:], data)
		received[i] = true
		complete++
		if err = requestMore(); err != nil {
			return Manifest{}, invalid("metadata_request_failed", err)
		}
	}
	return ParseInfo(metadata, identity, limits)
}

func readFrame(conn net.Conn) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, invalid("peer_read_failed", err)
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > maxPeerFrame {
		return nil, invalid("peer_frame_limit", ErrLimitExceeded)
	}
	if n == 0 {
		return nil, nil
	}
	var kind [1]byte
	if _, err := io.ReadFull(conn, kind[:]); err != nil {
		return nil, invalid("peer_read_failed", err)
	}
	// Unsolicited media data is never read into a payload buffer. The caller
	// closes the connection on this error; only BEP9 data is admitted.
	if kind[0] == 7 {
		return nil, invalid("media_payload_denied", ErrUnsupported)
	}
	frame := make([]byte, int(n))
	frame[0] = kind[0]
	if _, err := io.ReadFull(conn, frame[1:]); err != nil {
		return nil, invalid("peer_read_failed", err)
	}
	return frame, nil
}

func writeExtension(conn net.Conn, id byte, payload []byte) error {
	frame := make([]byte, 6+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)+2))
	frame[4], frame[5] = 20, id
	copy(frame[6:], payload)
	return writeAll(conn, frame)
}

func writeAll(w io.Writer, raw []byte) error {
	for len(raw) > 0 {
		n, err := w.Write(raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	return nil
}
