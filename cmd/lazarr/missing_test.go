package main

import (
	"testing"

	"github.com/your-username/lazarr/internal/config"
)

func TestMissingCollector_Wiring(t *testing.T) {
	c := config.Default()
	if missingCollector(c) != nil {
		t.Fatal("no playback.arrs: the collector must be off")
	}
	c.Playback.Arrs = []config.Arr{
		{Category: "sonarr_main", URL: "http://sonarr_main:8989", APIKey: "k", AlsoCategories: []string{"sonarr_main_seerr"}},
		{Category: "radarr_main", URL: "http://radarr_main:7878"}, // no key: skipped
		{Category: "radarr_4k", URL: "http://radarr_4k:7878", APIKey: "k"},
	}
	mc := missingCollector(c)
	if mc == nil {
		t.Fatal("arrs with keys: the collector must be on")
	}
	snap := mc.Snapshot()
	if len(snap.Arrs) != 2 || snap.Arrs[0].Category != "sonarr_main" || snap.Arrs[0].Kind != "sonarr" ||
		snap.Arrs[1].Category != "radarr_4k" || snap.Arrs[1].Kind != "radarr" {
		t.Fatalf("arrs = %+v, want sonarr_main (sonarr) + radarr_4k (radarr), one entry per instance", snap.Arrs)
	}
	c.Missing.Enabled = false
	if missingCollector(c) != nil {
		t.Fatal("missing.enabled false: the collector must be off")
	}
}
