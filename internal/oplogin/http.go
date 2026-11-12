package oplogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	openpayments "github.com/interledger/open-payments-go"
)

const (
	maxBody        = 1 << 20
	defaultTimeout = 15 * time.Second
)

var errBodyTooLarge = errors.New("open payments response too large")

// hardened returns a copy of base (or a default client) that never follows
// redirects, has a timeout, and fails reads of bodies over maxBody. base is
// not modified.
func hardened(base *http.Client) *http.Client {
	var c http.Client
	if base != nil {
		c = *base
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = cappedTransport{rt}
	return &c
}

type cappedTransport struct{ rt http.RoundTripper }

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, left: maxBody}
	return resp, nil
}

// cappedBody passes through up to left bytes, then errors if the body has
// any more.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, errBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// sanitize turns SDK and transport errors into messages that carry at most
// the method, host and status. Response bodies, paths, tokens and nonces
// never survive. Context errors stay matchable with errors.Is.
func sanitize(err error) error {
	var ce *openpayments.OpenPaymentsClientError
	if errors.As(err, &ce) {
		return fmt.Errorf("open payments %s %s: status %d", ce.Method, hostOf(ce.URL), ce.Status)
	}
	var cause error
	switch {
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	case errors.Is(err, errBodyTooLarge):
		cause = errBodyTooLarge
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if cause == nil {
			cause = ue.Err
		}
		return fmt.Errorf("open payments %s %s: %w", strings.ToUpper(ue.Op), hostOf(ue.URL), cause)
	}
	if cause != nil {
		return fmt.Errorf("open payments request failed: %w", cause)
	}
	return errors.New("open payments request failed")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "unknown-host"
	}
	return u.Host
}
