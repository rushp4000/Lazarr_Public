package qbit

import "time"

// PollWaitDownloadsForTest runs one wait-poller tick synchronously (the 20s ticker
// is impractical in unit tests). Compiled only for tests.
func PollWaitDownloadsForTest(s Server) {
	if srv, ok := s.(*server); ok {
		srv.pollWaitDownloads()
	}
}

// SetClockForTest replaces the server's clock so wait-budget tests can step time.
func SetClockForTest(s Server, now func() time.Time) {
	if srv, ok := s.(*server); ok {
		srv.now = now
	}
}

// SetArrFailDelaysForTest replaces the wait_fail_blocklist retry schedule.
func SetArrFailDelaysForTest(s Server, d []time.Duration) {
	if srv, ok := s.(*server); ok {
		srv.fails.delays = d
	}
}

// WaitArrFailsForTest blocks until every background wait_fail_blocklist call is done.
func WaitArrFailsForTest(s Server) {
	if srv, ok := s.(*server); ok {
		srv.fails.wg.Wait()
	}
}

// SlowTickForTest runs one slow-lane worker tick synchronously.
func SlowTickForTest(s Server) {
	if srv, ok := s.(*server); ok {
		srv.slowTick()
	}
}

// BencodeForTest encodes a synthetic .torrent (see slowlane_torrent_test.go).
var BencodeForTest = bencodeForTest
