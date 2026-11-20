package qbit

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // the BitTorrent v1 infohash is SHA-1
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bencodeForTest encodes string / []byte / int / []any / map[string]any (keys sorted,
// as the spec requires) for building synthetic .torrent files.
func bencodeForTest(v any) []byte {
	var b bytes.Buffer
	var enc func(v any)
	enc = func(v any) {
		switch x := v.(type) {
		case string:
			fmt.Fprintf(&b, "%d:%s", len(x), x)
		case []byte:
			fmt.Fprintf(&b, "%d:", len(x))
			b.Write(x)
		case int:
			fmt.Fprintf(&b, "i%de", x)
		case []any:
			b.WriteByte('l')
			for _, e := range x {
				enc(e)
			}
			b.WriteByte('e')
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('d')
			for _, k := range keys {
				enc(k)
				enc(x[k])
			}
			b.WriteByte('e')
		default:
			panic(fmt.Sprintf("bencodeForTest: %T", v))
		}
	}
	enc(v)
	return b.Bytes()
}

// testInfo is a v1 single-file info dict; mut tweaks it.
func testInfo(name string, mut func(map[string]any)) map[string]any {
	info := map[string]any{
		"name":         name,
		"length":       1 << 30,
		"piece length": 1 << 20,
		"pieces":       strings.Repeat("\x01", 20),
	}
	if mut != nil {
		mut(info)
	}
	return info
}

func testTorrent(info map[string]any, top map[string]any) []byte {
	t := map[string]any{"info": info, "created by": "lazarr-test"}
	for k, v := range top {
		t[k] = v
	}
	return bencodeForTest(t)
}

func TestParseTorrentBytes_HashIsSHA1OfInfo(t *testing.T) {
	info := testInfo("Lindy Mercer S01-S02 DVDRip", nil)
	raw := testTorrent(info, map[string]any{"comment": "padding after info: zzz"})
	want := sha1.Sum(bencodeForTest(info)) //nolint:gosec
	m, err := parseTorrentBytes(raw)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", want), m.Hash)
	assert.Equal(t, "Lindy Mercer S01-S02 DVDRip", m.Name)
	assert.False(t, m.Private)
	assert.False(t, m.V2Only)

	// parseTorrentFile (qBit torrents/add) agrees.
	h, n, err := parseTorrentFile(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, m.Hash, h)
	assert.Equal(t, m.Name, n)
}

func TestParseTorrentBytes_Trackers(t *testing.T) {
	raw := testTorrent(testInfo("Show", nil), map[string]any{
		"announce": "udp://tracker.one:1337/announce",
		"announce-list": []any{
			[]any{"udp://tracker.one:1337/announce", "https://tracker.two/announce"},
			[]any{"magnet:?not-a-tracker", "udp://tracker.three:80", "has space://x", ""},
			[]any{"https://tracker.two/announce", 42},
			"not-a-tier",
		},
	})
	m, err := parseTorrentBytes(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"udp://tracker.one:1337/announce", "https://tracker.two/announce", "udp://tracker.three:80"},
		m.Trackers, "announce first, then announce-list tiers; deduplicated; junk dropped")
}

func TestParseTorrentBytes_TrackerCap(t *testing.T) {
	var tier []any
	for i := 0; i < maxMagnetTrackers+20; i++ {
		tier = append(tier, fmt.Sprintf("udp://t%d.example:80", i))
	}
	m, err := parseTorrentBytes(testTorrent(testInfo("Show", nil), map[string]any{"announce-list": []any{tier}}))
	require.NoError(t, err)
	assert.Len(t, m.Trackers, maxMagnetTrackers)
}

func TestParseSlowTorrent(t *testing.T) {
	trackers := []any{[]any{"udp://tracker.one:1337/announce", "https://tracker.two/announce?x=1&y=2"}}
	name := "Old Show S01 & S02 (1999) [DVD]"
	raw := testTorrent(testInfo(name, nil), map[string]any{"announce-list": trackers})
	add, err := parseSlowTorrent(raw)
	require.NoError(t, err)
	want := fmt.Sprintf("%x", sha1.Sum(bencodeForTest(testInfo(name, nil)))) //nolint:gosec
	assert.Equal(t, want, add.Hash)
	assert.Equal(t, name, add.Name)
	assert.Equal(t, "uploaded .torrent (2 trackers)", add.Note)
	assert.True(t, strings.HasPrefix(add.Magnet, "magnet:?xt=urn:btih:"+want+"&dn="), add.Magnet)
	assert.Contains(t, add.Magnet, "&tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce")
	assert.Contains(t, add.Magnet, "&tr=https%3A%2F%2Ftracker.two%2Fannounce%3Fx%3D1%26y%3D2")
	assert.NotContains(t, add.Magnet, "+", "spaces are %20")

	// The built magnet round-trips through the magnet parser the rest of Lazarr uses.
	h, n := parseMagnet(add.Magnet)
	assert.Equal(t, want, h)
	assert.Equal(t, name, n)
}

func TestParseSlowTorrent_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{"private", testTorrent(testInfo("Private", func(i map[string]any) { i["private"] = 1 }), nil), "private torrent refused"},
		{"v2 only", testTorrent(testInfo("V2", func(i map[string]any) {
			delete(i, "pieces")
			i["meta version"] = 2
			i["file tree"] = map[string]any{}
		}), nil), "v2-only"},
		{"no info", bencodeForTest(map[string]any{"announce": "udp://x:1"}), "not a valid .torrent"},
		{"bad bencode", []byte("d4:infod4:name3:abc"), "not a valid .torrent"},
		{"not bencode", []byte("<html>login required</html>"), "not a valid .torrent"},
		{"empty", nil, "not a valid .torrent"},
		{"oversize", append([]byte("d4:info"), bytes.Repeat([]byte("x"), maxTorrentBytes)...), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSlowTorrent(tc.raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParseSlowTorrent_PrivateZeroAndHybridAccepted(t *testing.T) {
	add, err := parseSlowTorrent(testTorrent(testInfo("Public", func(i map[string]any) { i["private"] = 0 }), nil))
	require.NoError(t, err)
	assert.Equal(t, "uploaded .torrent (no trackers: DHT only)", add.Note)

	// Hybrid v1+v2: v1 pieces present, so SHA-1(info) is the btih TorBox needs.
	hybrid := testInfo("Hybrid", func(i map[string]any) {
		i["meta version"] = 2
		i["file tree"] = map[string]any{}
	})
	add, err = parseSlowTorrent(testTorrent(hybrid, nil))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", sha1.Sum(bencodeForTest(hybrid))), add.Hash) //nolint:gosec
}
