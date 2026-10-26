package sources

// This file runs the poll sources. A poll source reads an HTTP endpoint
// at an interval and turns each item of the answer into a run, through
// the same claims and the same publish as a webhook. Every replica of the
// front polls, and the claims keep the runs single. A poll that fails
// waits longer before the next one, twice as long after each failure, up
// to maxBackoff. No credential enters a log line or a counter: an error
// names the host of the endpoint and never its address in full.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"
)

// Limits of a poll.
const (
	// maxAnswer is the largest answer of an endpoint.
	maxAnswer = 8 << 20
	// maxBackoff is the longest pause after failed polls, unless the
	// interval of the source is longer.
	maxBackoff = 30 * time.Minute
	// firstPoll bounds the pause before the first poll.
	firstPoll = 5 * time.Second
)

// poller reads the endpoint of one poll source.
type poller struct {
	url     string
	host    string
	header  string // the header of the credential; "" means Authorization: Bearer
	value   string // the credential, or ""
	every   time.Duration
	timeout time.Duration
	first   time.Duration // bounds the pause before the first poll
	client  *http.Client
}

func newPoller(p Poll, value string) *poller {
	host := p.URL
	if u, err := url.Parse(p.URL); err == nil {
		host = u.Host
	}
	return &poller{url: p.URL, host: host, header: p.Header, value: value, every: p.Every, timeout: p.Timeout,
		first: firstPoll, client: &http.Client{CheckRedirect: sameHost}}
}

// sameHost follows a redirect only to the same scheme and host, so a
// credential in a header of the poll never reaches another host.
func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("the endpoint redirects more than 5 times")
	}
	if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return errors.New("the endpoint redirects to another host")
	}
	return nil
}

// fetch reads the answer of the endpoint.
func (p *poller) fetch(ctx context.Context) ([]byte, error) {
	body, err := get(ctx, p.client, p.url, p.header, p.value, p.timeout, maxAnswer)
	if err != nil {
		return nil, fmt.Errorf("sources: poll %s: %w", p.host, err)
	}
	return body, nil
}

// get reads the answer of an endpoint with one GET within timeout: at most
// limit bytes of a 2xx answer. The credential value goes in header, or as
// "Authorization: Bearer <value>" when header is empty, and no header
// when value is empty. An error holds no address and no credential.
func get(ctx context.Context, client *http.Client, rawURL, header, value string, timeout time.Duration, limit int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, redact(err)
	}
	req.Header.Set("Accept", "application/json")
	if value != "" {
		if header == "" {
			req.Header.Set("Authorization", "Bearer "+value)
		} else {
			req.Header.Set(header, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("the endpoint answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read the answer: %w", redact(err))
	}
	if len(body) > limit {
		return nil, fmt.Errorf("the answer is larger than %d bytes", limit)
	}
	return body, nil
}

// polls runs one poll source until Close.
func (s *Sources) polls(src *source) {
	defer s.wg.Done()
	p := src.poll
	// The replicas of a front often start together. A random pause before
	// the first poll spreads their polls.
	delay := rand.N(min(p.every, p.first))
	failures := 0
	for {
		t := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if Adopted(s.a, src.cfg.Name) {
			// A duty runs this entry now (duty.go), so the source polls no
			// more, and the duty polls in its place.
			failures, delay = 0, p.every
			continue
		}
		if err := s.pollOnce(s.ctx, src); err != nil {
			if s.ctx.Err() != nil {
				return
			}
			failures++
			delay = backoff(p.every, failures)
			s.log.Warn("sources: a poll failed", "source", src.cfg.Name, "failures", failures, "next", delay, "error", err)
		} else {
			failures, delay = 0, p.every
		}
		// A spread of a tenth keeps the replicas apart.
		delay += rand.N(delay/10 + 1)
	}
}

// backoff is the pause after a number of failed polls in a row: the
// interval, doubled for each failure, up to maxBackoff or the interval
// when that is longer.
func backoff(every time.Duration, failures int) time.Duration {
	limit := max(every, maxBackoff)
	d := every
	for i := 0; i < failures && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// pollOnce reads the endpoint once and delivers its items. An item that
// does not fit the templates counts as refused, and the other items go
// on. It returns an error when the endpoint, its answer, the store, or the
// queue failed.
func (s *Sources) pollOnce(ctx context.Context, src *source) error {
	body, err := src.poll.fetch(ctx)
	if err != nil {
		src.count.failed(err.Error(), s.now())
		return err
	}
	src.count.polled(s.now())
	events, err := decodeEvents(body, src.cfg.Items)
	if err != nil {
		err = fmt.Errorf("sources: poll %s: %w", src.poll.host, err)
		src.count.failed(err.Error(), s.now())
		return err
	}
	items := make([]rendered, 0, len(events))
	for i, e := range events {
		it, err := src.render(i+1, e)
		if err != nil {
			reason := fmt.Sprintf("item %d: %s", i+1, err)
			src.count.refused(1, reason, s.now())
			s.log.Warn("sources: an item does not fit the templates", "source", src.cfg.Name, "reason", reason)
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil
	}
	return s.deliver(ctx, src, items).err
}
