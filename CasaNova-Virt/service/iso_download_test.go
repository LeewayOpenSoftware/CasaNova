package service

// The ISO downloader reported nothing. Progress lived only in the bytes on
// disk and failures only in the log, so a 3 GiB image on a slow link looked
// exactly like one that had died ten minutes earlier — and there was no way to
// stop one that was going nowhere. The map behind it held struct{}: enough to
// answer "is this busy" and nothing else.
//
// These drive the real transfer against a test server: progress advances,
// cancel stops it and leaves no partial file, a failure stays visible with its
// reason, and a terminal entry does not block a retry.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"time"
)

// waitFor polls until cond holds. Every wait here is on a goroutine reaching a
// state, so a sleep would either be flaky or slow; this is neither.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func findDownload(s *DomainService, name string) (model.ISODownload, bool) {
	for _, x := range s.ISODownloads() {
		if x.Name == name {
			return x, true
		}
	}
	return model.ISODownload{}, false
}

func newTestService(t *testing.T) *DomainService {
	t.Helper()
	return NewDomainService(nil, t.TempDir())
}

// slowISO serves `total` bytes, pausing so a test can observe a transfer in
// flight rather than one that finished before the first poll.
func slowISO(total int, chunkPause time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1024)
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(chunkPause)
		}
	}))
}

func TestISODownload_ReportsProgressAndCompletes(t *testing.T) {
	srv := slowISO(64*1024, 2*time.Millisecond)
	defer srv.Close()
	s := newTestService(t)

	if err := s.DownloadISO(srv.URL+"/x.iso", "probe.iso"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Total comes from Content-Length, which is what makes a percentage
	// possible at all.
	waitFor(t, "the total to be known", func() bool {
		d, ok := findDownload(s, "probe.iso")
		return ok && d.TotalBytes == 64*1024
	})
	waitFor(t, "bytes to be counted", func() bool {
		d, ok := findDownload(s, "probe.iso")
		return ok && d.DoneBytes > 0
	})
	waitFor(t, "completion", func() bool {
		d, ok := findDownload(s, "probe.iso")
		return ok && d.Done && d.Error == ""
	})

	if _, err := os.Stat(filepath.Join(s.diskDir, "probe.iso")); err != nil {
		t.Errorf("the finished image is not in place: %v", err)
	}
}

func TestISODownload_CancelStopsItAndLeavesNothingBehind(t *testing.T) {
	// Big and slow enough that it cannot finish before the cancel lands.
	srv := slowISO(8*1024*1024, 5*time.Millisecond)
	defer srv.Close()
	s := newTestService(t)

	if err := s.DownloadISO(srv.URL+"/big.iso", "big.iso"); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the transfer to be under way", func() bool {
		d, ok := findDownload(s, "big.iso")
		return ok && d.DoneBytes > 0
	})

	if err := s.CancelISODownload("big.iso"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, "the entry to go terminal", func() bool {
		d, ok := findDownload(s, "big.iso")
		return ok && d.Done
	})

	d, _ := findDownload(s, "big.iso")
	if !d.Cancelled {
		t.Error("a cancelled transfer must say so; otherwise it reads as a failure")
	}
	// A cancel the operator asked for is not an error to report back at them.
	if d.Error != "" {
		t.Errorf("cancelling reported an error: %q", d.Error)
	}
	if _, err := os.Stat(filepath.Join(s.diskDir, "big.iso.part")); !os.IsNotExist(err) {
		t.Error("the partial file survived the cancel")
	}
	if _, err := os.Stat(filepath.Join(s.diskDir, "big.iso")); !os.IsNotExist(err) {
		t.Error("a cancelled download must not produce a finished image")
	}
}

func TestISODownload_AFailureStaysVisibleWithItsReason(t *testing.T) {
	// A download that simply disappears from the list looks identical whether
	// it finished or died — and the failure is the one the operator must see.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	s := newTestService(t)

	if err := s.DownloadISO(srv.URL+"/missing.iso", "missing.iso"); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the failure to be recorded", func() bool {
		d, ok := findDownload(s, "missing.iso")
		return ok && d.Done
	})

	d, _ := findDownload(s, "missing.iso")
	if d.Error == "" {
		t.Fatal("a failed download must carry a reason")
	}
	if !strings.Contains(d.Error, "404") {
		t.Errorf("the reason should name what the server said, got %q", d.Error)
	}
}

func TestISODownload_RefusesASecondCopyOfOneInFlight(t *testing.T) {
	// Two transfers of one name share a ".part" and interleave their writes,
	// which silently corrupts the image.
	srv := slowISO(4*1024*1024, 5*time.Millisecond)
	defer srv.Close()
	s := newTestService(t)

	if err := s.DownloadISO(srv.URL+"/dup.iso", "dup.iso"); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the transfer to be under way", func() bool {
		d, ok := findDownload(s, "dup.iso")
		return ok && d.DoneBytes > 0
	})
	if err := s.DownloadISO(srv.URL+"/dup.iso", "dup.iso"); err == nil {
		t.Fatal("a second download of the same name was allowed")
	}
	_ = s.CancelISODownload("dup.iso")
}

func TestISODownload_ATerminalEntryDoesNotBlockARetry(t *testing.T) {
	// The entry lingers so the UI can show the outcome. If that also held the
	// claim, one failure would lock the name out for as long as it lingered.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := slowISO(4096, 0)
	defer good.Close()
	s := newTestService(t)

	if err := s.DownloadISO(bad.URL+"/retry.iso", "retry.iso"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	waitFor(t, "the first attempt to fail", func() bool {
		d, ok := findDownload(s, "retry.iso")
		return ok && d.Done
	})

	if err := s.DownloadISO(good.URL+"/retry.iso", "retry.iso"); err != nil {
		t.Fatalf("a retry after a failure was refused: %v", err)
	}
	waitFor(t, "the retry to succeed", func() bool {
		d, ok := findDownload(s, "retry.iso")
		return ok && d.Done && d.Error == ""
	})
}

func TestCancel_RefusesANameThatIsNotDownloading(t *testing.T) {
	// Guards the guard: if cancel succeeded unconditionally, the cancel test
	// above would pass without cancelling anything.
	s := newTestService(t)
	if err := s.CancelISODownload("nothing.iso"); err == nil {
		t.Error("cancelling a download that does not exist must fail")
	}
}
