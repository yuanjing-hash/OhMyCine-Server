package resourcecontent

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func encoded(v any) []byte {
	var b bytes.Buffer
	var write func(any)
	write = func(v any) {
		switch x := v.(type) {
		case string:
			fmt.Fprintf(&b, "%d:", len(x))
			b.WriteString(x)
		case []byte:
			fmt.Fprintf(&b, "%d:", len(x))
			b.Write(x)
		case int:
			fmt.Fprintf(&b, "i%de", x)
		case int64:
			fmt.Fprintf(&b, "i%de", x)
		case []any:
			b.WriteByte('l')
			for _, item := range x {
				write(item)
			}
			b.WriteByte('e')
		case map[string]any:
			b.WriteByte('d')
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				write(k)
				write(x[k])
			}
			b.WriteByte('e')
		default:
			panic(fmt.Sprintf("unsupported fixture %T", v))
		}
	}
	write(v)
	return b.Bytes()
}

func singleInfo(name string, size int64) map[string]any {
	return map[string]any{"name": name, "length": size, "piece length": int64(16384), "pieces": bytes.Repeat([]byte{1}, int(pieceCount(size, 16384))*20)}
}

func torrentFixture(info map[string]any) []byte { return encoded(map[string]any{"info": info}) }

func infoIdentity(raw []byte) Identity {
	h := sha1.Sum(raw)
	return Identity{V1: hex.EncodeToString(h[:])}
}

func TestParseV1OriginalHashOrderAndOwnership(t *testing.T) {
	// An otherwise valid unsorted info dictionary must be hashed as received,
	// never re-encoded into the fixture helper's canonical ordering.
	rawInfo := []byte("d4:name9:movie.mkv6:lengthi123e12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
	raw := append(append([]byte("d4:info"), rawInfo...), 'e')
	m, err := ParseTorrent(raw, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Identity != infoIdentity(rawInfo) || m.Name != "movie.mkv" || m.TotalSize != 123 || m.MultiFile {
		t.Fatalf("bad single manifest: %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].Index != 0 || m.Files[0].Path != "movie.mkv" {
		t.Fatal(m.Files)
	}
	copyRaw := bytes.Clone(m.InfoBytes)
	for i := range raw {
		raw[i] = 0
	}
	if !bytes.Equal(copyRaw, m.InfoBytes) {
		t.Fatal("input aliases stored info")
	}
	multi := map[string]any{"name": "Series", "piece length": 16384, "pieces": bytes.Repeat([]byte{2}, 20), "files": []any{
		map[string]any{"length": int64(30), "path": []any{"Season 01", "02.mkv"}},
		map[string]any{"length": int64(40), "path": []any{"Season 01", "01.mkv"}},
	}}
	m, err = ParseTorrent(torrentFixture(multi), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Files[0].Path != "Season 01/02.mkv" || m.Files[1].Path != "Season 01/01.mkv" || m.Files[1].Index != 1 {
		t.Fatal("v1 selection indices reordered", m.Files)
	}
	verified, err := ParseInfo(m.InfoBytes, m.Identity, Limits{})
	if err != nil || !reflect.DeepEqual(verified.Files, m.Files) {
		t.Fatalf("peer info differs: %v", err)
	}
	if _, err = ParseInfo(m.InfoBytes, Identity{V1: strings.Repeat("0", 40)}, Limits{}); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
}

func TestParseV2AndHybrid(t *testing.T) {
	leaf := func(size int64) map[string]any {
		return map[string]any{"": map[string]any{"length": size, "pieces root": bytes.Repeat([]byte{4}, 32)}}
	}
	v2 := map[string]any{"name": "Series", "meta version": 2, "piece length": 16384, "file tree": map[string]any{"z.mkv": leaf(6), "a.mkv": leaf(7)}}
	rawInfo := encoded(v2)
	h2 := sha256.Sum256(rawInfo)
	m, err := ParseTorrent(torrentFixture(v2), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != "v2" || m.Identity.V1 != "" || m.Identity.V2 != hex.EncodeToString(h2[:]) || m.Files[0].Path != "a.mkv" || m.TotalSize != 13 || !m.MultiFile {
		t.Fatalf("bad v2 %+v", m)
	}
	if _, err = ParseInfo(rawInfo, m.Identity, Limits{}); err != nil {
		t.Fatal(err)
	}
	// A hybrid must retain padding offsets and original v1 indices while
	// matching the actual non-padding file set with the v2 tree.
	v2["files"] = []any{
		map[string]any{"length": int64(7), "path": []any{"a.mkv"}},
		map[string]any{"length": int64(16377), "path": []any{".pad", "16377"}, "attr": "p"},
		map[string]any{"length": int64(6), "path": []any{"z.mkv"}},
	}
	v2["pieces"] = bytes.Repeat([]byte{3}, 40)
	m, err = ParseTorrent(torrentFixture(v2), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	h1 := sha1.Sum(encoded(v2))
	h2 = sha256.Sum256(encoded(v2))
	if m.Format != "hybrid" || m.Identity != (Identity{V1: hex.EncodeToString(h1[:]), V2: hex.EncodeToString(h2[:])}) || !m.Files[1].Padding || m.Files[2].Index != 2 || m.Files[2].PiecesRoot == "" {
		t.Fatalf("bad hybrid %+v", m)
	}
	v2["files"].([]any)[2].(map[string]any)["length"] = int64(5)
	if _, err = ParseTorrent(torrentFixture(v2), Limits{}); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatal("hybrid mismatch accepted", err)
	}
	zero := map[string]any{"name": "empty", "meta version": 2, "piece length": 16384, "file tree": map[string]any{"empty": map[string]any{"": map[string]any{"length": int64(0)}}}}
	m, err = ParseTorrent(torrentFixture(zero), Limits{})
	if err != nil || m.MultiFile || m.TotalSize != 0 {
		t.Fatalf("zero v2: %+v %v", m, err)
	}
}

func TestTorrentValidation(t *testing.T) {
	tests := []struct {
		name   string
		info   map[string]any
		limits Limits
		want   error
	}{
		{"negative", singleInfo("x", 0), Limits{}, ErrInvalidMetainfo},
		{"name traversal", singleInfo("../x", 0), Limits{}, ErrUnsafePath},
		{"name windows", singleInfo("C:\\x", 0), Limits{}, ErrUnsafePath},
		{"name empty", singleInfo("", 0), Limits{}, ErrUnsafePath},
		{"name control", singleInfo("x\n", 0), Limits{}, ErrUnsafePath},
		{"name utf8", singleInfo(string([]byte{255}), 0), Limits{}, ErrUnsafePath},
		{"short path limit", singleInfo("xxxx", 0), Limits{MaxPathBytes: 3}, ErrUnsafePath},
		{"pieces mismatch", singleInfo("x", 1), Limits{}, ErrInvalidMetainfo},
		{"info limit", singleInfo("x", 1), Limits{MaxInfoBytes: 16}, ErrLimitExceeded},
		{"meta limit", singleInfo("x", 1), Limits{MaxMetainfoBytes: 16}, ErrLimitExceeded},
		{"depth", singleInfo("x", 1), Limits{MaxDepth: 1}, ErrLimitExceeded},
		{"value limit", singleInfo("x", 1), Limits{MaxValues: 2}, ErrLimitExceeded},
	}
	tests[0].info["length"] = int64(-1)
	tests[7].info["pieces"] = []byte{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTorrent(torrentFixture(tt.info), tt.limits)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v want %v", err, tt.want)
			}
		})
	}
	for _, badPath := range []string{"..", ".", "/abs", "a/b", "a\\b", "C:", ""} {
		info := map[string]any{"name": "n", "piece length": 16384, "pieces": "", "files": []any{map[string]any{"length": 0, "path": []any{badPath}}}}
		if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("accepted %q: %v", badPath, err)
		}
	}
	info := map[string]any{"name": "n", "piece length": int64(math.MaxInt64), "pieces": "", "files": []any{map[string]any{"length": int64(math.MaxInt64), "path": []any{"a"}}, map[string]any{"length": 1, "path": []any{"b"}}}}
	info["piece length"] = 16384
	if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatal("overflow", err)
	}
	info["files"] = []any{map[string]any{"length": 0, "path": []any{"a"}}, map[string]any{"length": 0, "path": []any{"a", "b"}}}
	if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("file/directory collision", err)
	}
	info["files"] = []any{map[string]any{"length": 0, "path": []any{"a"}}, map[string]any{"length": 0, "path": []any{"a-b"}}, map[string]any{"length": 0, "path": []any{"a", "b"}}}
	if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("nonadjacent ancestor collision", err)
	}
	info["files"] = []any{map[string]any{"length": 0, "path": []any{"a"}}, map[string]any{"length": 0, "path": []any{"a"}}}
	if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("duplicate", err)
	}
	if _, err := ParseTorrent(torrentFixture(info), Limits{MaxFiles: 1}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("file limit", err)
	}
	info["files"] = []any{map[string]any{"length": 0, "path": []any{"a"}, "symlink path": []any{"target"}}}
	if _, err := ParseTorrent(torrentFixture(info), Limits{}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("symlink", err)
	}
	for _, raw := range []string{"d4:infode4:infoee", "d4:info", "i03e", "i-0e", "i9223372036854775808e", "d4:infoi1eeX", "01:x"} {
		if _, err := ParseTorrent([]byte(raw), Limits{}); err == nil {
			t.Fatalf("accepted malformed %q", raw)
		}
	}
}

func TestMagnetAndPrivateFacts(t *testing.T) {
	hash := bytes.Repeat([]byte{9}, 20)
	h := hex.EncodeToString(hash)
	m, err := ParseMagnet("magnet:?xt=urn:btih:" + base32.StdEncoding.EncodeToString(hash) + "&tr=https%3A%2F%2Ftracker.example%2Fsecret&xs=http%3A%2F%2F127.0.0.1%2Fevil&x.pe=8.8.8.8%3A123")
	if err != nil || m.Identity.V1 != h || len(m.Trackers) != 1 || len(m.Peers) != 1 {
		t.Fatalf("magnet: %+v %v", m, err)
	}
	v2 := strings.Repeat("a1", 32)
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + h + "&xt=urn:btmh:1220" + v2)
	if err != nil || m.Identity.V2 != v2 {
		t.Fatal(err, m)
	}
	for _, raw := range []string{"magnet:?xt=urn:btih:abc", "magnet:?xt=urn:btmh:1220abc", "magnet:?xt=urn:btih:" + h + "&xt=urn:btih:" + strings.Repeat("0", 40), "http://example.com/x", "magnet:?xt=urn:btih:" + h + "&tr=file%3A%2F%2Fx"} {
		if _, err := ParseMagnet(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	info := singleInfo("movie.mkv", 1)
	info["private"] = 1
	mf, err := ParseTorrent(encoded(map[string]any{"info": info, "announce": "https://tracker.example/hidden-passkey", "url-list": "https://127.0.0.1/webseed"}), Limits{})
	if err != nil || !mf.Private || len(mf.Trackers) != 1 {
		t.Fatal(err)
	}
	dto, _ := json.Marshal(mf)
	if bytes.Contains(dto, []byte("hidden-passkey")) || bytes.Contains(dto, []byte("InfoBytes")) || bytes.Contains(dto, []byte("MetainfoBytes")) {
		t.Fatal("private facts escaped DTO", string(dto))
	}
}

func FuzzParseTorrent(f *testing.F) {
	f.Add(torrentFixture(singleInfo("seed.mkv", 0)))
	f.Add([]byte("d4:infodee"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 65536 {
			t.Skip()
		}
		_, _ = ParseTorrent(raw, Limits{MaxMetainfoBytes: 65536, MaxInfoBytes: 32768, MaxFiles: 64, MaxValues: 2048, MaxDepth: 16})
	})
}
