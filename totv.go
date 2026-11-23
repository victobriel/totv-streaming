// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// download streams GET path into dest (mode 0600), at most max bytes. 404 -> errNotFound.
func (c *totvClient) download(ctx context.Context, path, dest string, max int64) (int64, error) {
	res, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return 0, errors.New("unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return 0, errNotFound
	}
	if res.StatusCode != 200 {
		return 0, fmt.Errorf("status %d", res.StatusCode)
	}
	// refuse before writing a byte: the declared size must fit the disk with room to spare (unknown size: assume the worst, max)
	need := res.ContentLength
	if need < 0 || need > max {
		need = max
	}
	if res.ContentLength > max {
		return 0, errors.New("too large")
	}
	if err := checkSpace(filepath.Dir(dest), need+spaceSlack); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(res.Body, max+1))
	if err != nil {
		return n, errors.New("read failed")
	}
	if n > max {
		return n, errors.New("too large")
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
