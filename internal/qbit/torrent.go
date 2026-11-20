package qbit

import (
	"crypto/sha1" //nolint:gosec // sha1 is required by the BitTorrent protocol
	"fmt"
	"strconv"
	"strings"
)

// sha1Sum computes the SHA-1 digest of b (required by the BitTorrent infohash spec).
func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b) //nolint:gosec
	return h[:]
}

// findInfoDict locates the raw bytes of the "info" value in a bencode-encoded torrent.
// Returns start and end byte offsets (end is exclusive) and whether it was found.
//
// Bencode format reference:
//
//	string  → <length>:<bytes>
//	integer → i<decimal>e
//	list    → l<items>e
//	dict    → d<key><value>...e  (keys are bencode strings, sorted)
func findInfoDict(buf []byte) (start, end int, found bool) {
	// Walk the top-level dict looking for key "4:info".
	pos := 0
	if pos >= len(buf) || buf[pos] != 'd' {
		return 0, 0, false
	}
	pos++ // skip 'd'

	for pos < len(buf) && buf[pos] != 'e' {
		// Read key (bencode string).
		keyStart := pos
		keyEnd, ok := bencodeStringEnd(buf, pos)
		if !ok {
			return 0, 0, false
		}
		keyBytes := bencodeStringValue(buf, keyStart)
		pos = keyEnd

		// Value starts at pos.
		valStart := pos
		valEnd, ok2 := bencodeSkip(buf, pos)
		if !ok2 {
			return 0, 0, false
		}

		if string(keyBytes) == "info" {
			return valStart, valEnd, true
		}
		pos = valEnd
	}
	return 0, 0, false
}

// bencodeStringEnd returns the position AFTER a bencode string at buf[pos].
func bencodeStringEnd(buf []byte, pos int) (end int, ok bool) {
	colon := -1
	for i := pos; i < len(buf); i++ {
		if buf[i] == ':' {
			colon = i
			break
		}
		if buf[i] < '0' || buf[i] > '9' {
			return 0, false
		}
	}
	if colon < 0 {
		return 0, false
	}
	var length int
	if _, err := fmt.Sscanf(string(buf[pos:colon]), "%d", &length); err != nil {
		return 0, false
	}
	// Overflow-safe bound: compare length against the remaining buffer rather
	// than computing colon+1+length (which can overflow for a hostile length
	// near MaxInt and wrap negative, defeating an `end > len(buf)` check).
	if length < 0 || length > len(buf)-(colon+1) {
		return 0, false
	}
	return colon + 1 + length, true
}

// bencodeStringValue returns the raw string bytes of a bencode string at buf[pos].
func bencodeStringValue(buf []byte, pos int) []byte {
	colon := -1
	for i := pos; i < len(buf); i++ {
		if buf[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return nil
	}
	var length int
	if _, err := fmt.Sscanf(string(buf[pos:colon]), "%d", &length); err != nil {
		return nil
	}
	start := colon + 1
	// Overflow-safe bound (see bencodeStringEnd).
	if length < 0 || length > len(buf)-start {
		return nil
	}
	return buf[start : start+length]
}

// maxBencodeDepth bounds nested list/dict recursion so a hostile .torrent with
// deeply nested containers cannot exhaust the goroutine stack (a fatal,
// unrecoverable crash). Real torrents nest only a few levels.
const maxBencodeDepth = 128

// bencodeSkip returns the position after a single bencode value at buf[pos].
func bencodeSkip(buf []byte, pos int) (end int, ok bool) {
	return bencodeSkipDepth(buf, pos, 0)
}

func bencodeSkipDepth(buf []byte, pos, depth int) (end int, ok bool) {
	if depth > maxBencodeDepth {
		return 0, false
	}
	if pos >= len(buf) {
		return 0, false
	}
	switch {
	case buf[pos] == 'i':
		// integer: i<decimal>e
		for i := pos + 1; i < len(buf); i++ {
			if buf[i] == 'e' {
				return i + 1, true
			}
		}
		return 0, false
	case buf[pos] == 'l':
		// list
		pos++
		for pos < len(buf) && buf[pos] != 'e' {
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
		}
		if pos >= len(buf) {
			return 0, false
		}
		return pos + 1, true
	case buf[pos] == 'd':
		// dict
		pos++
		for pos < len(buf) && buf[pos] != 'e' {
			// key
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
			// value
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
		}
		if pos >= len(buf) {
			return 0, false
		}
		return pos + 1, true
	case buf[pos] >= '0' && buf[pos] <= '9':
		// string
		end, ok2 := bencodeStringEnd(buf, pos)
		return end, ok2
	default:
		return 0, false
	}
}

// torrentMeta is what Lazarr reads from a .torrent file.
type torrentMeta struct {
	Hash     string   // v1 infohash: lowercase hex SHA-1 of the raw bencoded info dict
	Name     string   // info.name
	Trackers []string // announce + announce-list, deduplicated, first-seen order
	Private  bool     // info.private == 1 (BEP 27)
	V2Only   bool     // BitTorrent v2 without v1 piece hashes (BEP 52): no btih exists
}

// maxMagnetTrackers caps the trackers carried into a built magnet: enough for any real
// swarm, small enough that a tracker-stuffed .torrent cannot bloat the stored magnet.
const maxMagnetTrackers = 50

// parseTorrentBytes reads the metadata of a bencoded .torrent. The info dict must be
// well-formed (it is what the infohash is computed over); trackers are best-effort.
func parseTorrentBytes(buf []byte) (torrentMeta, error) {
	var m torrentMeta
	if len(buf) > maxTorrentBytes {
		return m, fmt.Errorf("torrent: file exceeds %d bytes", maxTorrentBytes)
	}
	infoStart, infoEnd, found := findInfoDict(buf)
	if !found {
		return m, fmt.Errorf("torrent: info dict not found")
	}
	info := buf[infoStart:infoEnd]
	m.Hash = fmt.Sprintf("%x", sha1Sum(info))

	var metaVersion int64
	hasPieces := false
	bencodeDictEach(info, func(key string, val []byte) {
		switch key {
		case "name":
			m.Name = string(bencodeStr(val))
		case "private":
			n, _ := bencodeInt(val)
			m.Private = n == 1
		case "meta version":
			metaVersion, _ = bencodeInt(val)
		case "pieces":
			hasPieces = true
		}
	})
	// A hybrid torrent carries v1 pieces next to the v2 file tree, so SHA-1(info) is a
	// real btih; a v2-only one has no v1 identity at all.
	m.V2Only = metaVersion >= 2 && !hasPieces

	seen := map[string]bool{}
	add := func(v []byte) {
		tr := string(bencodeStr(v))
		if !usableTracker(tr) || seen[tr] || len(m.Trackers) >= maxMagnetTrackers {
			return
		}
		seen[tr] = true
		m.Trackers = append(m.Trackers, tr)
	}
	bencodeDictEach(buf, func(key string, val []byte) {
		switch key {
		case "announce":
			add(val)
		case "announce-list": // list of tiers, each a list of URLs (BEP 12)
			bencodeListEach(val, func(tier []byte) { bencodeListEach(tier, add) })
		}
	})
	return m, nil
}

// usableTracker reports whether s looks like a tracker URL worth handing to TorBox.
func usableTracker(s string) bool {
	if s == "" || len(s) > 1024 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return false
		}
	}
	l := strings.ToLower(s)
	for _, p := range []string{"udp://", "http://", "https://", "ws://", "wss://"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// bencodeDictEach calls fn for every key/value of the bencode dict in buf (value as raw
// bencode bytes). It stops silently at the first malformed entry.
func bencodeDictEach(buf []byte, fn func(key string, val []byte)) {
	if len(buf) == 0 || buf[0] != 'd' {
		return
	}
	pos := 1
	for pos < len(buf) && buf[pos] != 'e' {
		keyEnd, ok := bencodeStringEnd(buf, pos)
		if !ok {
			return
		}
		key := bencodeStringValue(buf, pos)
		valEnd, ok := bencodeSkip(buf, keyEnd)
		if !ok {
			return
		}
		fn(string(key), buf[keyEnd:valEnd])
		pos = valEnd
	}
}

// bencodeListEach calls fn for every element of the bencode list in buf. It stops
// silently at the first malformed element.
func bencodeListEach(buf []byte, fn func(val []byte)) {
	if len(buf) == 0 || buf[0] != 'l' {
		return
	}
	pos := 1
	for pos < len(buf) && buf[pos] != 'e' {
		end, ok := bencodeSkip(buf, pos)
		if !ok {
			return
		}
		fn(buf[pos:end])
		pos = end
	}
}

// bencodeStr returns the bytes of the bencode string val, or nil when val is not one.
func bencodeStr(val []byte) []byte {
	if len(val) == 0 || val[0] < '0' || val[0] > '9' {
		return nil
	}
	return bencodeStringValue(val, 0)
}

// bencodeInt returns the bencode integer val (i<decimal>e).
func bencodeInt(val []byte) (int64, bool) {
	if len(val) < 3 || val[0] != 'i' || val[len(val)-1] != 'e' {
		return 0, false
	}
	n, err := strconv.ParseInt(string(val[1:len(val)-1]), 10, 64)
	return n, err == nil
}
