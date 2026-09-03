// methods in this package automatically include a context with a timeout, in order to solve the problem of hanging requests and to avoid goroutine leaks
package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// cancelOnCloseBody wraps an http.Response.Body so that calling Close()
// also cancels the request context. The deadline therefore remains
// active for the entire lifetime of the read — not just until Do()
// returns.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc

	// declaredLength is the response's Content-Length: >= 0 when the upstream
	// declared one, -1 when it did not (chunked or streaming). Close only
	// drains when this is a small, known number.
	declaredLength int64
}

// maxDrainOnClose bounds how much of an unread body Close will consume so the
// connection can go back in the pool. Peer responses here are small JSON
// envelopes; anything larger is not worth blocking a Close for.
const maxDrainOnClose = 16 << 10

func (b *cancelOnCloseBody) Close() error {
	// DRAIN BEFORE CLOSING, OR THE CONNECTION IS THROWN AWAY.
	//
	// net/http returns a connection to the idle pool only when its body has
	// been read to EOF and then closed. Every peer client in this tree does
	//
	//	defer response.Body.Close()
	//	if response.StatusCode != http.StatusOK { ... }
	//
	// — it reads the status and never touches the body. So the transport
	// discarded the connection every single time. Measured with a counting
	// listener: 200 calls produced 200 TCP connections without the drain and
	// 1 with it. Every notify, every share deletion, and every one of the
	// route watchdog's re-registrations (51 prefixes, every 30 s, for the life
	// of the box) paid a full handshake and teardown.
	//
	// The fix lives HERE rather than at the call sites on purpose: there are
	// eight of them across five files, and this codebase's most repeated defect
	// is fixing one caller and believing the class is closed.
	//
	// ONLY WHEN IT IS SAFE TO. Draining an arbitrary body would turn a Close
	// that used to return instantly into one that can block for the whole
	// request timeout — a slow or streaming upstream that the caller
	// deliberately abandoned would now be waited on. So the drain happens only
	// when the response declared a length AND that length is small: then we
	// know exactly what we are about to read, and it is already in flight.
	// Anything else closes as before, which is the current behaviour, so this
	// cannot make any case worse than it is today.
	if b.declaredLength >= 0 && b.declaredLength <= maxDrainOnClose {
		_, _ = io.CopyN(io.Discard, b.ReadCloser, b.declaredLength+1)
	}
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Do issues an HTTP request with a per-call timeout and returns the
// response. The caller is responsible for closing response.Body.
//
// Wave 33-A HIGH: previously the body was naked
//
//	ctx, cancel := context.WithTimeout(...)
//	defer cancel()
//	...
//	return http.DefaultClient.Do(request)
//
// `defer cancel()` fires the moment Do returns, which cancels the
// context the *http.Response.Body still depends on. Small responses
// that fit in the kernel buffer happened to work; large responses,
// chunked streaming, and slow upstreams aborted mid-read with
// "context canceled". Every cross-service call (gateway probes,
// MessageBus publish, AppManagement registry pulls) shares this
// helper, so the bug surfaced as flaky inter-service errors that
// vanished when load dropped. Fix: keep the cancel alive until the
// caller actually closes the body.
func Do(requestFunc func(ctx context.Context) (*http.Request, error), timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	request, err := requestFunc(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}

	response.Body = &cancelOnCloseBody{
		ReadCloser: response.Body, cancel: cancel, declaredLength: response.ContentLength,
	}
	return response, nil
}

func Get(url string, timeout time.Duration) (*http.Response, error) {
	return GetWithHeader(url, timeout, nil)
}

func GetWithHeader(url string, timeout time.Duration, header map[string]string) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}

		for k, v := range header {
			request.Header.Set(k, v)
		}
		return request, nil
	}, timeout)
}

// default header "Content-Type: application/json" is included.
func Post(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	return PostWithHeader(url, body, timeout, nil)
}

// default header "Content-Type: application/json" is included.
func PostWithHeader(url string, body []byte, timeout time.Duration, header map[string]string) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}

		request.Header.Set("Content-Type", "application/json")

		for k, v := range header {
			request.Header.Set(k, v)
		}

		return request, nil
	}, timeout)
}

// default header "Content-Type: application/json" is included.
func Put(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	return PutWithHeader(url, body, timeout, nil)
}

// default header "Content-Type: application/json" is included.
func PutWithHeader(url string, body []byte, timeout time.Duration, header map[string]string) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("accept", "application/json")
		request.Header.Set("Content-Type", "application/json")

		for k, v := range header {
			request.Header.Set(k, v)
		}

		return request, nil
	}, timeout)
}
func PutByReader(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("accept", "application/json")
		request.Header.Set("Content-Type", "application/json")

		return request, nil
	}, timeout)
}

// default header "Content-Type: application/json" is included.
func Delete(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}

		request.Header.Set("Content-Type", "application/json")

		return request, nil
	}, timeout)
}

// DeleteWithHeader is Delete plus caller-supplied headers, used for the internal
// secret header on peer-to-peer calls.
func DeleteWithHeader(url string, body []byte, timeout time.Duration, header map[string]string) (*http.Response, error) {
	return Do(func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			request.Header.Set(k, v)
		}
		return request, nil
	}, timeout)
}
