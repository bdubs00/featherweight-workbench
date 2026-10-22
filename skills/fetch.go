package skills

// This file fetches the files of a skill and the index of a marketplace
// over HTTP (docs/ARCHITECTURE.md section 20.7). Every fetch has a timeout
// and a bound of 4 MB, follows at most three redirects, and refuses an
// address that is private, loopback, link-local, or otherwise not on the
// public internet, at the moment of the dial, so a name that resolves
// again to another address is caught too. A repository is read through
// the routes of its host for an archive or a raw file, never with a git
// binary, and nothing that the front fetches runs.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"
)

// Bounds of a fetch.
const (
	// MaxFetchBytes bounds one answer of a host.
	MaxFetchBytes = 4 << 20
	// MaxRedirects bounds the redirects of one fetch.
	MaxRedirects = 3
	// FetchTimeout bounds one fetch.
	FetchTimeout = 30 * time.Second
	// maxListed bounds the files that a listing of a repository reads.
	maxListed = 20000
)

// FetchError is a fetch that failed. Status is the status that a route
// answers: 400 for a URL that the front refuses, 413 for an answer over
// the bound, and 502 for a host that fails.
type FetchError struct {
	Status int
	Text   string
}

func (e *FetchError) Error() string { return "skills: " + e.Text }

func fetchErr(status int, format string, args ...any) error {
	return &FetchError{Status: status, Text: fmt.Sprintf(format, args...)}
}

// errPrivate is the refusal of the dialer.
var errPrivate = errors.New("the address is not on the public internet")

// Fetcher fetches from the hosts of the skills. The zero value is not
// ready; NewFetcher returns one. The fields name the hosts of GitHub, so a
// test serves them; AllowPrivate lets a test reach a server on the
// loopback address.
type Fetcher struct {
	Client       *http.Client
	GitHubHost   string // the host of the pages of repositories
	GitHubAPI    string // the base of the API
	GitHubRaw    string // the base of the raw files
	AllowPrivate bool
}

// NewFetcher returns a fetcher for the public internet.
func NewFetcher() *Fetcher {
	f := &Fetcher{GitHubHost: "github.com", GitHubAPI: "https://api.github.com", GitHubRaw: "https://raw.githubusercontent.com"}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: f.control}
	f.Client = &http.Client{
		Timeout: FetchTimeout,
		Transport: &http.Transport{
			// A proxy of the environment would see every address as its
			// own, so the front dials the host itself.
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > MaxRedirects {
				return fetchErr(400, "the host redirected more than %d times", MaxRedirects)
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return fetchErr(400, "the host redirected to a URL that is not http or https")
			}
			if req.URL.User != nil {
				return fetchErr(400, "the host redirected to a URL with a user name or a password")
			}
			return nil
		},
	}
	return f
}

// control refuses an address that is not on the public internet.
func (f *Fetcher) control(_, address string, _ syscall.RawConn) error {
	if f.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !public(ip) {
		return fmt.Errorf("%w: %s", errPrivate, ip)
	}
	return nil
}

// nonPublic lists the ranges beside the ones that netip names.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("100::/64"),
}

// public reports whether an address is on the public internet.
func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// checkURL parses a URL that an admin gives: http or https, a host, and
// no user name or password.
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fetchErr(400, "%q is no URL", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fetchErr(400, "the URL %s is not http or https", u.Redacted())
	}
	if u.User != nil {
		return nil, fetchErr(400, "the URL holds a user name or a password; the front sends no credential to a host of skills")
	}
	return u, nil
}

// get fetches a URL and returns the body and its content type.
func (f *Fetcher) get(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := checkURL(raw)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fetchErr(400, "%s: %v", u.Redacted(), err)
	}
	req.Header.Set("User-Agent", "fw-workbench-skills")
	resp, err := f.Client.Do(req)
	if err != nil {
		var fe *FetchError
		switch {
		case errors.As(err, &fe):
			return nil, "", fe
		case errors.Is(err, errPrivate):
			return nil, "", fetchErr(400, "%s resolves to an address that is not on the public internet, and the front fetches no skill from there", u.Host)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, "", fetchErr(502, "%s did not answer within %s", u.Host, FetchTimeout)
		}
		return nil, "", fetchErr(502, "%s did not answer: %v", u.Host, cleanErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fetchErr(502, "%s answered %d for %s", u.Host, resp.StatusCode, u.Redacted())
	}
	if resp.ContentLength > MaxFetchBytes {
		return nil, "", fetchErr(413, "%s is larger than %d bytes", u.Redacted(), MaxFetchBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBytes+1))
	if err != nil {
		return nil, "", fetchErr(502, "read %s: %v", u.Redacted(), cleanErr(err))
	}
	if len(b) > MaxFetchBytes {
		return nil, "", fetchErr(413, "%s is larger than %d bytes", u.Redacted(), MaxFetchBytes)
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// cleanErr is the innermost words of an error of the client.
func cleanErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// repo is a repository of a git host, and a directory in it.
type repo struct {
	github  bool   // read through the API and the raw files of GitHub
	owner   string // GitHub
	name    string // GitHub
	ref     string
	dir     string // a directory of the repository, "" for its root
	archive string // the URL of the zip archive of another host
	web     string // the URL of the repository, for the record of a source
}

// repoOf reads the URL of a repository: a URL of GitHub, a URL that ends
// in .git, and a URL of a directory on a host of the forms
// /<owner>/<repo>/src/branch/<ref>/<dir> and /<owner>/<repo>/-/tree/<ref>/<dir>.
// A fragment names the ref of a URL that ends in .git. It reports false
// for any other URL.
func (f *Fetcher) repoOf(u *url.URL) (repo, bool) {
	parts := splitPath(u.Path)
	if strings.EqualFold(u.Host, f.GitHubHost) && len(parts) >= 2 {
		r := repo{github: true, owner: parts[0], name: strings.TrimSuffix(parts[1], ".git"), ref: "HEAD"}
		if u.Fragment != "" {
			r.ref = u.Fragment
		}
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
			r.ref = parts[3]
			r.dir = strings.Join(parts[4:], "/")
			if parts[2] == "blob" {
				r.dir = path.Dir(r.dir)
				if r.dir == "." {
					r.dir = ""
				}
			}
		} else if len(parts) > 2 {
			return repo{}, false
		}
		r.web = "https://" + f.GitHubHost + "/" + r.owner + "/" + r.name
		return r, true
	}
	base := *u
	base.RawQuery, base.Fragment = "", ""
	switch {
	case strings.HasSuffix(u.Path, ".git") && len(parts) >= 2:
		ref := u.Fragment
		if ref == "" {
			ref = "main"
		}
		base.Path = strings.TrimSuffix(u.Path, ".git")
		return repo{ref: ref, archive: base.String() + "/archive/" + url.PathEscape(ref) + ".zip", web: base.String()}, true
	case len(parts) >= 5 && parts[2] == "src" && parts[3] == "branch":
		ref := parts[4]
		base.Path = "/" + parts[0] + "/" + parts[1]
		return repo{ref: ref, dir: strings.Join(parts[5:], "/"), archive: base.String() + "/archive/" + url.PathEscape(ref) + ".zip", web: base.String()}, true
	case len(parts) >= 5 && parts[2] == "-" && parts[3] == "tree":
		ref := parts[4]
		base.Path = "/" + parts[0] + "/" + parts[1]
		return repo{ref: ref, dir: strings.Join(parts[5:], "/"), archive: base.String() + "/-/archive/" + url.PathEscape(ref) + "/" + url.PathEscape(parts[1]+"-"+ref) + ".zip", web: base.String()}, true
	}
	return repo{}, false
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tree is the files of a repository: each path, and its text when the
// listing read it, such as from an archive.
type tree struct {
	paths []string
	text  map[string][]byte
}

// list lists the files of a repository: the tree of the API of GitHub, or
// the entries of the zip archive of another host.
func (f *Fetcher) list(ctx context.Context, r repo) (tree, error) {
	if r.github {
		u := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", strings.TrimSuffix(f.GitHubAPI, "/"), url.PathEscape(r.owner), url.PathEscape(r.name), url.PathEscape(r.ref))
		b, _, err := f.get(ctx, u)
		if err != nil {
			return tree{}, err
		}
		var t struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"tree"`
			Truncated bool `json:"truncated"`
		}
		if err := json.Unmarshal(b, &t); err != nil {
			return tree{}, fetchErr(502, "the listing of %s/%s is not JSON", r.owner, r.name)
		}
		var out tree
		for _, e := range t.Tree {
			if e.Type == "blob" && len(out.paths) < maxListed {
				out.paths = append(out.paths, e.Path)
			}
		}
		return out, nil
	}
	b, _, err := f.get(ctx, r.archive)
	if err != nil {
		return tree{}, err
	}
	return unzipTree(b)
}

// unzipTree reads the files of a zip archive. An archive whose entries
// share one top directory, as the archive of a repository does, loses
// that directory. Each file is read up to the bound of a fetch.
func unzipTree(b []byte) (tree, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return tree{}, fetchErr(400, "the archive is no zip file: %v", err)
	}
	out := tree{text: map[string][]byte{}}
	var names []string
	total := 0
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || len(names) >= maxListed {
			continue
		}
		if zf.UncompressedSize64 > MaxFetchBytes {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxFetchBytes+1))
		rc.Close()
		if err != nil || len(data) > MaxFetchBytes {
			continue
		}
		total += len(data)
		if total > 4*MaxFetchBytes {
			return tree{}, fetchErr(413, "the archive unpacks to more than %d bytes", 4*MaxFetchBytes)
		}
		names = append(names, zf.Name)
		out.text[zf.Name] = data
	}
	top := ""
	for i, n := range names {
		first, _, ok := strings.Cut(n, "/")
		if !ok {
			top = ""
			break
		}
		if i == 0 {
			top = first
		} else if first != top {
			top = ""
			break
		}
	}
	if top != "" {
		text := make(map[string][]byte, len(out.text))
		for _, n := range names {
			rel := strings.TrimPrefix(n, top+"/")
			text[rel] = out.text[n]
			out.paths = append(out.paths, rel)
		}
		out.text = text
		return out, nil
	}
	out.paths = names
	return out, nil
}

// read returns one file of a repository: from the listing when it holds
// the text, else from the raw files of GitHub.
func (f *Fetcher) read(ctx context.Context, r repo, t tree, p string) ([]byte, error) {
	if b, ok := t.text[p]; ok {
		return b, nil
	}
	if !r.github {
		return nil, fetchErr(502, "the archive holds no file %s", p)
	}
	u := fmt.Sprintf("%s/%s/%s/%s/%s", strings.TrimSuffix(f.GitHubRaw, "/"), url.PathEscape(r.owner), url.PathEscape(r.name), url.PathEscape(r.ref), escapePath(p))
	b, _, err := f.get(ctx, u)
	return b, err
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
