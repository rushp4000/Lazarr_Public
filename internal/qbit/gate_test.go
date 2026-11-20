package qbit

import (
	"testing"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

const mib = int64(1) << 20

func gatePolicy(archives string) config.Policy {
	p := config.Default().Policy
	p.RejectArchives = archives
	return p
}

func TestGateRefusal(t *testing.T) {
	ep := "Show.S01E01.1080p.WEB.H264-GRP/"
	cases := []struct {
		name     string
		archives string
		files    []torbox.CachedFile
		refuse   bool
	}{
		// The four live fakes of 2026-10-11/12 (sonarr_main, from RSS):
		// one .exe named like the episode.
		{"live: CAKES .exe 929,063,766 B", "only", []torbox.CachedFile{{Name: "Law.and.Ledger.S26E02.1080p.WEB.H264-CAKES.exe", Size: 929063766}}, true},
		{"live: MeGusta .exe 929,063,766 B", "only", []torbox.CachedFile{{Name: "Law.and.Ledger.Night.S28E02.1080p.HEVC.x265-MeGusta.exe", Size: 929063766}}, true},
		{"live: FLUX .exe 886,772,008 B", "only", []torbox.CachedFile{{Name: "Its.Always.Rainy.in.Portland.S18E10.The.Guys.Have.a.Sleepover.1080p.WEB.H264-FLUX.exe", Size: 886772008}}, true},
		{"exe next to a real video", "none", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "codec.EXE", Size: mib}}, true},
		{"script .lnk", "none", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "Play.lnk", Size: 1024}}, true},
		{"real episode + subs + nfo", "only", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "e.srt", Size: 40000}, {Name: ep + "e.nfo", Size: 100}}, false},
		{"season pack", "only", []torbox.CachedFile{{Name: "S01/e1.mkv", Size: 500 * mib}, {Name: "S01/e2.mkv", Size: 510 * mib}}, false},
		{"sample only", "only", []torbox.CachedFile{{Name: ep + "sample.mkv", Size: 30 * mib}}, true},
		{"no video at all", "only", []torbox.CachedFile{{Name: ep + "readme.txt", Size: 100}}, true},
		{"rar set only (only)", "only", []torbox.CachedFile{{Name: ep + "e.rar", Size: 50 * mib}, {Name: ep + "e.r00", Size: 50 * mib}}, true},
		{"rar set only (none) still has no video", "none", []torbox.CachedFile{{Name: ep + "e.rar", Size: 50 * mib}}, true},
		{"subs zip + video (only)", "only", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "subs.zip", Size: 2 * mib}}, false},
		{"subs zip + video (any)", "any", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "subs.zip", Size: 2 * mib}}, true},
		{"multi-volume part rar (any)", "any", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "e.part01.rar", Size: mib}}, true},
		{"r99 volume (any)", "any", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "e.r99", Size: mib}}, true},
		{"tgz (any)", "any", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "extras.tgz", Size: mib}}, true},
		// Live fake 2026-10-10 (radarr_main, imported): a lone .iso.
		{"live: SLOT lone .iso 1,760,816,711 B", "only", []torbox.CachedFile{{Name: "Dim Lights 2025.1080p.WEB.H264-SLOT/Dim Lights 2025.1080p.WEB.H264-SLOT.iso", Size: 1760816711}}, true},
		{"iso is not video and not archive (any)", "any", []torbox.CachedFile{{Name: "Movie.2009.BluRay.iso", Size: 40000 * mib}}, true},
		{"iso next to a real video (any)", "any", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "extras.iso", Size: 4000 * mib}}, false},
		// Genuine RARBG releases carry this file next to the video.
		{"RARBG_DO_NOT_MIRROR.exe + video", "only", []torbox.CachedFile{{Name: "Inkwell.S02E01.1080p.WEB.H264-GRP[rartv]/Inkwell.S02E01.mkv", Size: 1200 * mib}, {Name: "Inkwell.S02E01.1080p.WEB.H264-GRP[rartv]/RARBG_DO_NOT_MIRROR.exe", Size: 99 * 1024}}, false},
		{"RARBG_DO_NOT_MIRROR.exe alone", "only", []torbox.CachedFile{{Name: "x/RARBG_DO_NOT_MIRROR.exe", Size: 99 * 1024}}, true},
		{"other exe next to RARBG_DO_NOT_MIRROR.exe", "only", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "RARBG_DO_NOT_MIRROR.exe", Size: 99 * 1024}, {Name: ep + "setup.exe", Size: mib}}, true},
		{"7z + video (none)", "none", []torbox.CachedFile{{Name: ep + "e.mkv", Size: 900 * mib}, {Name: ep + "x.7z", Size: mib}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, file := gateRefusal(gatePolicy(tc.archives), tc.files)
			if (reason != "") != tc.refuse {
				t.Fatalf("refuse=%v, got reason %q (file %q)", tc.refuse, reason, file)
			}
		})
	}
}

func TestGateRefusal_DisabledAndMinZero(t *testing.T) {
	p := gatePolicy("only")
	p.ContentGate = false
	if r, _ := gateRefusal(p, []torbox.CachedFile{{Name: "x.exe", Size: 1}}); r != "" {
		t.Fatalf("content_gate=false must accept, got %q", r)
	}
	p = gatePolicy("only")
	p.MinVideoMiB = 0
	if r, _ := gateRefusal(p, []torbox.CachedFile{{Name: "e.mkv", Size: 1}}); r != "" {
		t.Fatalf("min_video_mib=0 accepts any video, got %q", r)
	}
	if r, _ := gateRefusal(p, []torbox.CachedFile{{Name: "e.rar", Size: 1}}); r == "" {
		t.Fatal("min_video_mib=0: archive-only must still be refused under only")
	}
}
