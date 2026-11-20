package qbit

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sevenTrackers is a realistic public tracker list (EZTV-style magnet).
var sevenTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://open.stealth.si:80/announce",
	"udp://explodie.org:6969/announce",
}

func magnetWithTrackers(sep string) string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:" + bbbHex + sep + "dn=" + url.QueryEscape("Lindy Mercer S01-S02 DVDRip x264"))
	for _, tr := range sevenTrackers {
		b.WriteString(sep + "tr=" + url.QueryEscape(tr))
	}
	return b.String()
}

func trackersOf(t *testing.T, magnet string) []string {
	t.Helper()
	q, err := url.ParseQuery(strings.TrimPrefix(magnet, "magnet:?"))
	require.NoError(t, err)
	return q["tr"]
}

func TestNormalizeMagnet(t *testing.T) {
	plain := magnetWithTrackers("&")
	cases := []struct {
		name, in string
	}{
		{"plain", plain},
		{"amp entity", magnetWithTrackers("&amp;")},
		{"amp entity upper", magnetWithTrackers("&AMP;")},
		{"numeric entity", magnetWithTrackers("&#38;")},
		{"zero-padded numeric entity", magnetWithTrackers("&#038;")},
		{"hex entity", magnetWithTrackers("&#x26;")},
		{"double escaped", magnetWithTrackers("&amp;amp;")},
		{"surrounding whitespace", "  \t" + magnetWithTrackers("&amp;") + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeMagnet(tc.in)
			assert.Equal(t, plain, got)
			h, n := parseMagnet(got)
			assert.Equal(t, bbbHex, h)
			assert.Equal(t, "Lindy Mercer S01-S02 DVDRip x264", n)
			assert.Equal(t, sevenTrackers, trackersOf(t, got), "every tracker survives")
		})
	}
}

func TestParseSlowInput_Magnets(t *testing.T) {
	b32 := bbbBase32(t)
	cases := []struct {
		name, in, wantName string
	}{
		{"plain magnet", "magnet:?xt=urn:btih:" + bbbHex + "&dn=Show.S01.720p", "Show.S01.720p"},
		{"&amp; magnet (EZTV API magnet_url)", "magnet:?xt=urn:btih:" + bbbHex + "&amp;dn=Show%20S01&amp;tr=udp%3A%2F%2Fx%3A1", "Show S01"},
		{"numeric-entity magnet", "magnet:?xt=urn:btih:" + bbbHex + "&#38;dn=Show+S01&#38;tr=udp%3A%2F%2Fx%3A1", "Show S01"},
		{"base32 btih", "magnet:?xt=urn:btih:" + b32 + "&dn=B32", "B32"},
		{"missing dn", "magnet:?xt=urn:btih:" + bbbHex + "&tr=udp%3A%2F%2Fx%3A1", ""},
		{"bare infohash", strings.ToUpper(bbbHex), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hash, name, magnet, err := parseSlowInput(tc.in)
			require.NoError(t, err)
			assert.Equal(t, bbbHex, hash)
			assert.Equal(t, tc.wantName, name)
			assert.NotContains(t, magnet, "amp;")
			assert.NotContains(t, magnet, "&#")
		})
	}

	_, _, magnet, err := parseSlowInput(magnetWithTrackers("&amp;"))
	require.NoError(t, err)
	assert.Equal(t, magnetWithTrackers("&"), magnet, "the stored magnet is the unescaped one")
	assert.Len(t, trackersOf(t, magnet), 7, "7 trackers survive the round-trip")

	_, _, _, err = parseSlowInput("magnet:?dn=no-hash")
	require.Error(t, err)
}
