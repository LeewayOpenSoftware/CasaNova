package v1

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// NO FILE-SERVING HANDLER MAY USE ctx.File.
//
// echo's ctx.File calls http.ServeContent(c.Response(), ...), and echo's
// *Response does not implement io.ReaderFrom. io.Copy therefore cannot take the
// fast path: it falls back to a 32 KiB buffer loop that reads every byte into
// user space and writes it out again. Handing ServeContent the underlying
// writer instead lets net/http use sendfile(2), where the bytes never leave the
// kernel.
//
// Measured with strace on a 64 MiB download:
//
//	ctx.File()                4,174 write               99 ms in syscalls
//	ctx.Response().Writer       156 sendfile + 4 write   11 ms
//
// This is a structural check because the cost is invisible at the call site:
// ctx.File is the obvious, idiomatic thing to write, and nothing about it warns
// that it costs 26x the syscalls.
func TestNoHandlerServesFilesThroughTheEchoWrapper(t *testing.T) {
	const file = "file.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%s is unreadable, so this census went blind: %v", file, err)
	}
	body := string(src)

	// ctx.File( / c.File( on a real call line, not inside a comment.
	call := regexp.MustCompile(`(?m)^[^/\n]*\bctx\.File\(`)
	for _, loc := range call.FindAllStringIndex(body, -1) {
		line := strings.Count(body[:loc[0]], "\n") + 1
		t.Errorf("%s:%d: ctx.File() serves through echo's *Response, which has no "+
			"ReadFrom, so the kernel sendfile path is unavailable and the download "+
			"runs as a 32 KiB copy loop (4,174 write syscalls per 64 MiB instead "+
			"of 156 sendfile). Use http.ServeContent(ctx.Response().Writer, ...).",
			file, line)
	}

	// The census must be able to see what it audits: if the file no longer
	// serves any content this way, the pattern has drifted and green means
	// nothing.
	if !strings.Contains(body, "http.ServeContent(ctx.Response().Writer") {
		t.Fatal("no handler serves through ctx.Response().Writer any more — this " +
			"census is not looking at the code it claims to audit")
	}
	if n := strings.Count(body, "http.ServeContent(ctx.Response().Writer"); n < 2 {
		t.Errorf("only %d handlers use the raw writer; both the single-file and "+
			"the batch download path should", n)
	}
}
