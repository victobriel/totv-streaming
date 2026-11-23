// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// totvClient talks to the Totv (serverUrl) with the session token. serverUrl is only accepted as
// http://127.0.0.1:<port><base>; redirects are never followed; bodies and tokens are never logged.
type totvClient struct {
	base  string
	token string
	hc    *http.Client
}

func newTotvClient(serverURL, token string) (*totvClient, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("bad serverUrl")
	}
	if p, err := strconv.Atoi(u.Port()); err != nil || p < 1 || p > 65535 {
		return nil, errors.New("bad serverUrl")
	}
	if strings.Contains(u.Path, "..") || strings.Contains(u.Path, "//") {
		return nil, errors.New("bad serverUrl")
	}
	if token == "" || len(token) > 512 || strings.ContainsAny(token, "\r\n ") {
		return nil, errors.New("bad token")
	}
	return &totvClient{
		base:  strings.TrimRight(u.String(), "/"),
		token: token,
		hc: &http.Client{
			Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *totvClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.hc.Do(req)
}

var errNotFound = errors.New("not found")

// downloadError carries a short, sanitized reason (no paths, no URLs, no token) for the log and the `detail` of the 502; Unwrap keeps
// errors.Is(errNotFound / errNoSpace) working.
type downloadError struct {
	reason string
	err    error
}

func (e *downloadError) Error() string { return e.reason }
func (e *downloadError) Unwrap() error { return e.err }

// downloadReason is the sanitized reason of a download failure ("" when err is not one of ours).
func downloadReason(err error) string {
	var de *downloadError
	if errors.As(err, &de) {
		return de.reason
	}
	return ""
}

// ioReason turns an I/O error into text without the file names a *PathError carries: ENOSPC/EDQUOT (a tmpfs with a per-user quota reports
// free space in statfs and still refuses the write), a timeout, a cut connection, or the errno text.
func ioReason(err error, what string, n int64) (string, bool) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return what + ": no space left on device after " + strconv.FormatInt(n>>20, 10) + " MiB", true
	case errors.Is(err, syscall.EDQUOT):
		return what + ": disk quota exceeded after " + strconv.FormatInt(n>>20, 10) + " MiB", true
	case errors.Is(err, context.DeadlineExceeded):
		return what + ": timeout after " + strconv.FormatInt(n>>20, 10) + " MiB", false
	case errors.Is(err, context.Canceled):
		return what + ": cancelled after " + strconv.FormatInt(n>>20, 10) + " MiB", false
	case errors.Is(err, io.ErrUnexpectedEOF):
		return what + ": connection closed early after " + strconv.FormatInt(n>>20, 10) + " MiB", false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return what + ": timeout after " + strconv.FormatInt(n>>20, 10) + " MiB", false
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		return what + ": " + en.Error() + " after " + strconv.FormatInt(n>>20, 10) + " MiB", false
	}
	return what + " failed after " + strconv.FormatInt(n>>20, 10) + " MiB", false
}

// download streams GET path into dest (mode 0600), at most max bytes. 404 -> errNotFound. Failures are *downloadError with a sanitized reason.
func (c *totvClient) download(ctx context.Context, path, dest string, max int64) (int64, error) {
	res, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		r := "totv unreachable"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			r, _ = ioReason(err, "request", 0)
		}
		return 0, &downloadError{reason: r, err: errors.New("unreachable")}
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return 0, &downloadError{reason: "http status 404", err: errNotFound}
	}
	if res.StatusCode != 200 {
		return 0, &downloadError{reason: fmt.Sprintf("http status %d", res.StatusCode), err: fmt.Errorf("status %d", res.StatusCode)}
	}
	// refuse before writing a byte: the declared size must fit the disk with room to spare (unknown size: assume the worst, max)
	need := res.ContentLength
	if need < 0 || need > max {
		need = max
	}
	if res.ContentLength > max {
		return 0, &downloadError{reason: "image of " + strconv.FormatInt(res.ContentLength>>20, 10) + " MiB is over the " + strconv.FormatInt(max>>20, 10) + " MiB limit", err: errors.New("too large")}
	}
	if err := checkSpace(filepath.Dir(dest), need+spaceSlack); err != nil {
		if errors.Is(err, errNoSpace) {
			return 0, &downloadError{reason: "not enough free space for " + strconv.FormatInt(need>>20, 10) + " MiB", err: err}
		}
		r, _ := ioReason(err, "statfs", 0)
		return 0, &downloadError{reason: r, err: err}
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		r, _ := ioReason(err, "create", 0)
		return 0, &downloadError{reason: r, err: err}
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(res.Body, max+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		r, full := ioReason(err, "download", n)
		if full {
			return n, &downloadError{reason: r, err: errNoSpace}
		}
		return n, &downloadError{reason: r, err: errors.New("read failed")}
	}
	if n > max {
		return n, &downloadError{reason: "image is over the " + strconv.FormatInt(max>>20, 10) + " MiB limit", err: errors.New("too large")}
	}
	if res.ContentLength >= 0 && n != res.ContentLength {
		return n, &downloadError{reason: "short body: " + strconv.FormatInt(n>>20, 10) + " of " + strconv.FormatInt(res.ContentLength>>20, 10) + " MiB", err: errors.New("short")}
	}
	return n, nil
}

func (c *totvClient) putSRAM(ctx context.Context, itemID int64, core string, mediaID int64, data []byte) error {
	q := url.Values{"core": {core}, "mediaId": {strconv.FormatInt(mediaID, 10)}}
	res, err := c.do(ctx, "PUT", "/api/games/"+strconv.FormatInt(itemID, 10)+"/sram?"+q.Encode(), data)
	if err != nil {
		return errors.New("unreachable")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return nil
}
