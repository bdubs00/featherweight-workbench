package fleet

// This file sends the packages of a pool to its running workers
// (docs/ARCHITECTURE.md section 14.11). A change of the setting of a pool
// replaces no worker: the hash of the pool leaves the setting out, and the
// front sends the setting to each worker that runs with PUT
// /v1/environment of the kernel (the kernel architecture, section 18).
// The kernel installs the difference in the background and keeps the
// Workspace and the runs of the Task. The front wakes no worker for it: a
// worker that sleeps gets the setting at the next pass after it wakes,
// because its next event names the old setting. The file also asks the
// index of the mirror for a new Python package before the front saves it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// EnvironmentPath is the route of the kernel that takes the setting of the
// packages of a worker.
const EnvironmentPath = "/v1/environment"

// envPushAgain is how long a pass waits before it sends the same setting
// to one worker again, while the worker installs it.
const envPushAgain = 2 * time.Minute

// envPushTimeout bounds one PUT /v1/environment to a worker.
const envPushTimeout = 15 * time.Second

// EnvironmentBody is the body of PUT /v1/environment: the switch, whether
// the agent can ask, and both lists. The kernel keeps it as the base of the
// environment of the worker and drops the local changes of the tool env.
type EnvironmentBody struct {
	On     bool     `json:"on"`
	Ask    bool     `json:"ask"`
	Python []string `json:"python"`
	Tools  []string `json:"tools"`
}

// environmentBody returns the body for the setting s.
func environmentBody(s pools.PackageSetting) EnvironmentBody {
	return EnvironmentBody{On: s.On, Ask: s.Ask, Python: pools.Names(s.Python), Tools: pools.Names(s.Tools)}
}

// envPushes remembers the last setting that the front sent to each worker,
// so a pass sends a setting again only after envPushAgain.
type envPushes struct {
	mu   sync.Mutex
	last map[string]envPushMark
}

type envPushMark struct {
	key string
	at  time.Time
}

// due reports whether the setting with key goes to the member now, and
// records it when it does. With force, it always goes.
func (e *envPushes) due(member, key string, now time.Time, force bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		e.last = map[string]envPushMark{}
	}
	if m, ok := e.last[member]; ok && !force && m.key == key && now.Sub(m.at) < envPushAgain {
		return false
	}
	e.last[member] = envPushMark{key: key, at: now}
	return true
}

// forget drops the mark of a member after a push that failed, so the next
// pass tries again.
func (e *envPushes) forget(member string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.last, member)
}

// pushEnvironment sends the setting s to the worker member with PUT
// /v1/environment and the static token of the front. It sends nothing to
// a worker that does not run, because a request through the router wakes
// a Task. Without force, it sends the same setting to one worker at most
// once in envPushAgain. It reports whether it sent.
func (p *platform) pushEnvironment(ctx context.Context, member string, s pools.PackageSetting, force bool) (bool, error) {
	p.mu.Lock()
	m := p.members[member]
	running := m != nil && m.phase == phaseRunning
	p.mu.Unlock()
	if !running {
		return false, nil
	}
	body := environmentBody(s)
	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	if !p.f.envPushed.due(member, string(raw), time.Now(), force) {
		return false, nil
	}
	if err := p.send(ctx, m, http.MethodPut, EnvironmentPath, raw); err != nil {
		p.f.envPushed.forget(member)
		return true, err
	}
	return true, nil
}

// send makes a request with a JSON body to a member through the router,
// with the static token of the front, and wants an answer of 2xx.
func (p *platform) send(ctx context.Context, m *platformMember, method, path string, body []byte) error {
	t, err := fw.EnvSecrets{}.Get(ctx, p.tokenEnv)
	if err != nil || strings.TrimSpace(t) == "" {
		return fmt.Errorf("no static token in the secret %s", p.tokenEnv)
	}
	cctx, cancel := context.WithTimeout(fw.WithCredential(ctx, "Bearer "+strings.TrimSpace(t)), envPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.f.Forward(cctx, m.name, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &answerError{path: path, code: resp.StatusCode}
	}
	return nil
}

// pushPool sends the setting s to each worker of the pool that runs, after
// a change of the setting, in the background. The log names each worker
// that did not take it. The next pass tries again for such a worker.
func (f *Fleet) pushPool(pool string, s pools.PackageSetting) {
	p := f.platform
	if p == nil || f.mirror == nil {
		return
	}
	p.mu.Lock()
	st := p.poolState[pool]
	p.mu.Unlock()
	for _, task := range st.Tasks {
		member := st.Atespace + "/" + task.Name
		go func() {
			ctx, cancel := context.WithTimeout(f.ctx, envPushTimeout)
			defer cancel()
			if sent, err := p.pushEnvironment(ctx, member, s, true); err != nil {
				f.log.Warn("fleet: send the packages to a worker", "pool", pool, "task", member, "error", err)
			} else if sent {
				f.log.Info("fleet: sent the packages to a worker", "pool", pool, "task", member, "on", s.On, "ask", s.Ask)
			}
		}()
	}
}

// syncEnvironments sends the setting of the pool p to each worker of the
// state st of a pass that runs the current configuration and whose last
// event since its creation names another switch, ask, or lists. A worker
// without such an event gets nothing, because its kernel publishes its
// state at its start. The caller holds no lock.
func (p *platform) syncEnvironments(ctx context.Context, pool *pools.Pool, st pools.Status) {
	f := p.f
	if f.mirror == nil || f.envStates == nil {
		return
	}
	s := pool.PackageSetting()
	for _, task := range st.Tasks {
		if !task.Current || task.Phase != phaseRunning {
			continue
		}
		member := pool.Atespace + "/" + task.Name
		rctx, cancel := context.WithTimeout(ctx, overlayTimeout)
		rec, ok, err := f.envState(rctx, member)
		cancel()
		if err != nil || !ok {
			continue
		}
		if created := p.createdOf(member); !created.IsZero() && rec.Time.Before(created) {
			continue
		}
		if envTook(rec, s) {
			continue
		}
		if sent, err := p.pushEnvironment(ctx, member, s, false); err != nil {
			f.log.Warn("fleet: send the packages to a worker", "pool", pool.Name, "task", member, "error", err)
		} else if sent {
			f.log.Info("fleet: sent the packages to a worker whose environment names another setting", "pool", pool.Name, "task", member)
		}
	}
}

// MirrorTimeout bounds the question to the index of the mirror about one
// Python package.
var MirrorTimeout = 3 * time.Second

// checkPython asks the index of the Python mirror for each name, at
// <mirror>/<normalized name>/. An answer of 404 refuses the name with
// "There is no Python package named X.". A mirror that does not answer,
// or that answers anything else, blocks nothing.
func (f *Fleet) checkPython(ctx context.Context, names []string) error {
	if f.mirror == nil || len(names) == 0 {
		return nil
	}
	client := &http.Client{Timeout: MirrorTimeout}
	base := strings.TrimRight(f.mirror.Mirrors.Python, "/")
	for _, name := range names {
		key := pools.PackageName(pools.ListPython, name)
		if key == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+key+"/", nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			f.log.Info("fleet: the Python mirror did not answer about a package; the front saves it", "package", name, "error", err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: There is no Python package named %s.", fw.ErrInvalidRequest, writtenName(name))
		}
	}
	return nil
}

// writtenName returns the name of the package of a requirement as a
// person wrote it, without the version: "PyYAML" of "PyYAML>=6".
func writtenName(req string) string {
	req = strings.TrimSpace(req)
	end := 0
	for end < len(req) {
		c := req[end]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			break
		}
		end++
	}
	if end == 0 {
		return req
	}
	return req[:end]
}
