package qbit

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// The add-time content gate (policy.content_gate, v1.2.8). checkcached returns the
// file list for free, so a release that cannot be a real movie/episode is refused at
// add time ("Fails.") and the arr falls through to its next release, instead of being
// accepted, failing import ("Found executable file with extension '.exe'") and sitting
// in the queue forever. Seen live 2026-10-11/12: four RSS grabs on sonarr_main that were
// a single 886-929 MB .exe named like the episode.

// executableExts are never part of a media release.
var executableExts = map[string]bool{
	".exe": true, ".scr": true, ".bat": true, ".cmd": true, ".lnk": true, ".msi": true,
	".com": true, ".vbs": true, ".js": true, ".jar": true, ".apk": true,
}

// benignExecutables are known tracker droppings that ship next to a real video and are
// ignored by the arrs' importer (lower-cased base names). 7 genuine imports in the 45 days
// before 2026-10-12 carried RARBG_DO_NOT_MIRROR.exe (Inkwell S02, The Quiet Neighbor,
// The Platform). They are skipped here; the video check below still applies.
var benignExecutables = map[string]bool{
	"rarbg_do_not_mirror.exe": true,
}

// videoExts are the container extensions that count as a real video. .iso is NOT one:
// Radarr imports disc images, but Plex can't play them, and a lone 1.76 GB .iso fake
// ("Dim Lights 2025.1080p.WEB.H264-SLOT") was imported on radarr_main on 2026-10-10.
// An .iso is neither video nor archive, so an .iso-only release fails the video check.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true,
	".ts": true, ".m2ts": true, ".mts": true, ".mpg": true, ".mpeg": true, ".webm": true,
	".flv": true, ".vob": true, ".divx": true, ".ogm": true,
}

// archiveExts plus multi-volume RAR parts (.r00-.r999; .partNN.rar ends in .rar anyway).
var archiveExts = map[string]bool{
	".rar": true, ".zip": true, ".7z": true, ".tar": true, ".gz": true, ".tgz": true,
}

var rarVolumeRE = regexp.MustCompile(`^\.r\d{2,3}$`)

// Archive policies (policy.reject_archives).
const (
	ArchivesNone = "none" // archives never cause a refusal on their own
	ArchivesOnly = "only" // refuse when there are archives and no qualifying video (default)
	ArchivesAny  = "any"  // refuse when ANY archive file is present
)

func fileExt(name string) string { return strings.ToLower(path.Ext(name)) }

func isArchive(name string) bool {
	ext := fileExt(name)
	return archiveExts[ext] || rarVolumeRE.MatchString(ext)
}

// gateRefusal returns why a release must be refused ("" = accept) and the file that
// decided it ("" when no single file did). Pure: unit-tested with the live fakes.
func gateRefusal(p config.Policy, files []torbox.CachedFile) (reason, file string) {
	if !p.ContentGate || len(files) == 0 {
		return "", ""
	}
	minBytes := int64(p.MinVideoMiB) << 20
	var firstArchive string
	qualifyingVideo := false
	for _, f := range files {
		ext := fileExt(f.Name)
		if executableExts[ext] && !benignExecutables[strings.ToLower(path.Base(f.Name))] {
			return "executable file (" + ext + ")", f.Name
		}
		if isArchive(f.Name) && firstArchive == "" {
			firstArchive = f.Name
		}
		if videoExts[ext] && f.Size >= minBytes {
			qualifyingVideo = true
		}
	}
	if firstArchive != "" {
		switch p.RejectArchives {
		case ArchivesAny:
			return "archive file (policy.reject_archives=any)", firstArchive
		case ArchivesNone:
			// fall through to the video check
		default: // "only" / ""
			if !qualifyingVideo {
				return "archives and no video file", firstArchive
			}
		}
	}
	if !qualifyingVideo && p.MinVideoMiB > 0 {
		return fmt.Sprintf("no video file >= %d MiB", p.MinVideoMiB), ""
	}
	return "", ""
}
