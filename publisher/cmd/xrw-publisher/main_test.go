package main

import (
	"testing"
	"time"
)

func TestPagesForDiscovery(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		lastFullScan     time.Time
		fullScanInterval time.Duration
		wantPages        int
		wantFullScan     bool
	}{
		{name: "first cycle scans everything", wantPages: 0, wantFullScan: true, fullScanInterval: 24 * time.Hour},
		{name: "normal cycle scans latest pages", lastFullScan: now.Add(-time.Hour), wantPages: 5, fullScanInterval: 24 * time.Hour},
		{name: "expired interval scans everything", lastFullScan: now.Add(-24 * time.Hour), wantPages: 0, wantFullScan: true, fullScanInterval: 24 * time.Hour},
		{name: "zero interval disables full scan", wantPages: 5, fullScanInterval: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pages, fullScan := pagesForDiscovery(now, test.lastFullScan, 5, test.fullScanInterval)
			if pages != test.wantPages || fullScan != test.wantFullScan {
				t.Fatalf("pagesForDiscovery() = (%d, %v), want (%d, %v)", pages, fullScan, test.wantPages, test.wantFullScan)
			}
		})
	}
}
