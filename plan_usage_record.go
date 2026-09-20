// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// 🎯T84: keep what the server actually said.
//
// Every plan-usage parser maps the fields it knows and drops the rest at
// parse time. That is how Anthropic's per-model weekly windows sat in each
// response for weeks, invisible, while the product reported that no such
// figure existed. It is also why "show me an earlier payload" could only be
// answered by issuing another request, on the one endpoint where issuing
// requests is what gets us refused.
//
// So the response body rides home on the reading. It is recorded at the
// transport, which means it covers every provider uniformly rather than
// each fetcher remembering to do it, and it is bounded: a runaway body
// must not be able to grow a snapshot without limit.

// PlanRawBodyLimit bounds a retained response body. Real plan-usage
// payloads are a few hundred bytes; this leaves room for a surface that
// grows without letting one become a memory or disk problem.
const PlanRawBodyLimit = 64 << 10

// planRecorder captures the last response of one fetch.
type planRecorder struct {
	mu     sync.Mutex
	status int
	body   []byte
}

func (r *planRecorder) observe(status int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
	r.body = body
}

func (r *planRecorder) result() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, string(r.body)
}

// planRecordingTransport copies each response body as it passes, then
// hands the caller an identical, unread body.
type planRecordingTransport struct {
	inner http.RoundTripper
	rec   *planRecorder
}

func (t *planRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Read through the limit, then splice whatever was read back in front
	// of the remainder so the fetcher sees an untouched stream.
	head, rerr := io.ReadAll(io.LimitReader(resp.Body, PlanRawBodyLimit))
	if rerr != nil {
		t.rec.observe(resp.StatusCode, head)
		resp.Body = io.NopCloser(bytes.NewReader(head))
		return resp, nil
	}
	t.rec.observe(resp.StatusCode, head)
	rest := resp.Body
	resp.Body = &planSplicedBody{Reader: io.MultiReader(bytes.NewReader(head), rest), closer: rest}
	return resp, nil
}

type planSplicedBody struct {
	io.Reader
	closer io.Closer
}

func (b *planSplicedBody) Close() error { return b.closer.Close() }

// recordingClient returns a client that copies responses into rec. The
// caller's own transport, timeout and jar are preserved; only the
// round-tripper is wrapped.
func recordingClient(c *http.Client, rec *planRecorder) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	cp := *c
	cp.Transport = &planRecordingTransport{inner: c.Transport, rec: rec}
	return &cp
}
