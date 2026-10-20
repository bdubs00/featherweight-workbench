package fleet

// This file reads the configuration of the fleet: the block fleet under the
// top-level key workbench of fw.yaml, which the package teams claims, and
// the variables FW_WORKBENCH_FLEET_*, which win over the file.

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvName     = "FW_WORKBENCH_FLEET_NAME"
	EnvInterval = "FW_WORKBENCH_FLEET_INTERVAL"
	EnvExpire   = "FW_WORKBENCH_FLEET_EXPIRE"
	// EnvAtespaces and EnvEgress name the sources of the view
	// (platform.go): the atespaces whose Tasks are members, with commas
	// between them, and the egress stream of the workers.
	EnvAtespaces = "FW_WORKBENCH_FLEET_ATESPACES"
	EnvEgress    = "FW_WORKBENCH_FLEET_EGRESS"
	// EnvIntake names the subject prefix of the intake queues of the workers
	// (enqueue.go).
	EnvIntake = "FW_WORKBENCH_FLEET_INTAKE"
	// EnvSuspendAfter is how long a Task stays idle before the front
	// suspends it, a duration such as 10m, or off (suspend.go).
	EnvSuspendAfter = "FW_WORKBENCH_FLEET_SUSPEND_AFTER"
	// EnvWakeOnQueue turns the wake on queue depth on or off (queues.go).
	EnvWakeOnQueue = "FW_WORKBENCH_FLEET_WAKE_ON_QUEUE"
)

// Config configures the fleet. The fleet is on when Atespaces or Egress
// names a source (On).
//
// Name is the name of this process in the view. The default is
// "<atespace>/<task>" under ax, else "<host>:<port>". Interval is the time
// between two reads of the Task list, and Expire is the age after which the
// view forgets a worker that only the stream knows and a finished run.
//
// Atespaces names the atespaces whose Tasks are members, read from the ax
// API at ax.server at every Interval, and Egress names the subject prefix
// of the egress stream of the workers, bus.egress on each of them
// (platform.go). Intake names the subject prefix of the intake queues of
// the workers: a worker that runs the profile <agent> sets bus.intake to
// "<intake>.<agent>", and the front puts the run requests of a job there
// (enqueue.go).
//
// SuspendAfter is how long a Task of those atespaces stays idle before the
// front suspends it (suspend.go). Nil means the default of ten minutes when
// Atespaces is set, and zero turns the controller off.
//
// WakeOnQueue lets the front wake the sleeping Tasks that serve a profile
// when jobs wait in the intake queue of that profile (queues.go). Nil means
// on when Atespaces and Intake are set, and false turns it off. The front
// still wakes the Tasks after its own enqueue.
type Config struct {
	Name      string        `yaml:"name"`
	Interval  time.Duration `yaml:"interval"`
	Expire    time.Duration `yaml:"expire"`
	Atespaces []string      `yaml:"atespaces"`
	Egress    string        `yaml:"egress"`
	Intake    string        `yaml:"intake"`

	SuspendAfter *time.Duration `yaml:"suspendAfter"`
	WakeOnQueue  *bool          `yaml:"wakeOnQueue"`
}

// On reports whether the configuration names a source of members.
func (c Config) On() bool { return len(c.Atespaces) > 0 || strings.TrimSpace(c.Egress) != "" }

// suspendAfter returns the grace period of the suspend controller, or zero
// when it is off.
func (c Config) suspendAfter() time.Duration {
	if c.SuspendAfter == nil {
		return 0
	}
	return *c.SuspendAfter
}

// wakeOnQueue reports whether the front wakes the sleeping Tasks for the
// jobs that wait in a queue.
func (c Config) wakeOnQueue() bool {
	return c.WakeOnQueue != nil && *c.WakeOnQueue
}

// block is the part of the workbench block that this package reads.
type block struct {
	Fleet Config `yaml:"fleet"`
}

// Load reads the configuration of the fleet from the block and the
// variables, fills the defaults, and checks the values. It returns an error
// that names the key for a value that does not fit.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(Block, &b); err != nil {
			return Config{}, err
		}
	}
	c := b.Fleet
	if v, ok := lookup(EnvName); ok {
		c.Name = v
	}
	if v, ok := lookup(EnvAtespaces); ok {
		c.Atespaces = nil
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				c.Atespaces = append(c.Atespaces, a)
			}
		}
	}
	if v, ok := lookup(EnvEgress); ok {
		c.Egress = v
	}
	if v, ok := lookup(EnvIntake); ok {
		c.Intake = v
	}
	if v, ok := lookup(EnvSuspendAfter); ok {
		if strings.EqualFold(v, "off") {
			v = "0s"
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("fleet: %s=%q is not a duration such as 10m, or off", EnvSuspendAfter, v)
		}
		c.SuspendAfter = &d
	}
	if v, ok := lookup(EnvWakeOnQueue); ok {
		on, err := parseSwitch(v)
		if err != nil {
			return Config{}, fmt.Errorf("fleet: %s=%q is not on or off", EnvWakeOnQueue, v)
		}
		c.WakeOnQueue = &on
	}
	for _, e := range []struct {
		env string
		dst *time.Duration
	}{{EnvInterval, &c.Interval}, {EnvExpire, &c.Expire}} {
		if v, ok := lookup(e.env); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("fleet: %s=%q is not a duration such as 30s or 2h", e.env, v)
			}
			*e.dst = d
		}
	}
	c.Name, c.Egress, c.Intake = strings.TrimSpace(c.Name), strings.TrimSpace(c.Egress), strings.TrimSpace(c.Intake)
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Expire <= 0 {
		c.Expire = DefaultExpire
	}
	if c.SuspendAfter == nil && len(c.Atespaces) > 0 {
		d := DefaultSuspendAfter
		c.SuspendAfter = &d
	}
	if c.WakeOnQueue == nil && len(c.Atespaces) > 0 && c.Intake != "" {
		on := true
		c.WakeOnQueue = &on
	}
	if c.On() {
		c.Name = defaults(c.Name, cfg)
	}
	if problems := check(c); len(problems) > 0 {
		return Config{}, fmt.Errorf("fleet: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// parseSwitch reads on, off, true, false, yes, no, 1, or 0.
func parseSwitch(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("fleet: %q is not on or off", v)
}

func lookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// defaults fills the name of this process. Under ax, the Task gives it.
// Elsewhere, the name is "<host>:<port>" from the host name and the port of
// the listener.
func defaults(name string, cfg *fw.Config) string {
	if name != "" {
		return name
	}
	if task, _ := ax.Stashed(); task != nil {
		if atespace, taskName := ax.Identity(task); atespace != "" && taskName != "" {
			return atespace + "/" + taskName
		}
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "fw"
	}
	if cfg != nil {
		if _, port, err := net.SplitHostPort(cfg.Listen); err == nil && port != "" {
			return host + ":" + port
		}
	}
	return host
}

// check lists the problems of a configuration: the name has no space and
// at most 128 characters; each subject prefix has no empty token, space, or
// wildcard; each atespace is a name; and the durations are not negative.
func check(c Config) []string {
	var p []string
	if n := c.Name; n != "" && (len(n) > 128 || strings.ContainsAny(n, " \t\r\n")) {
		p = append(p, fmt.Sprintf("workbench.fleet.name %q holds a space or is longer than 128 characters", n))
	}
	if c.Interval < 0 {
		p = append(p, fmt.Sprintf("workbench.fleet.interval %s is negative", c.Interval))
	}
	for _, s := range []struct{ key, val string }{{"egress", c.Egress}, {"intake", c.Intake}} {
		if s.val == "" {
			continue
		}
		for _, t := range strings.Split(s.val, ".") {
			if t == "" || strings.ContainsAny(t, " \t*>") {
				p = append(p, fmt.Sprintf("workbench.fleet.%s %q is not a subject prefix: it holds an empty token, a space, or a wildcard", s.key, s.val))
				break
			}
		}
	}
	for i, a := range c.Atespaces {
		if strings.TrimSpace(a) == "" || strings.ContainsAny(a, " \t/") {
			p = append(p, fmt.Sprintf("workbench.fleet.atespaces[%d] %q is not an atespace", i, a))
		}
	}
	if c.Expire < 0 {
		p = append(p, fmt.Sprintf("workbench.fleet.expire %s is negative", c.Expire))
	}
	if d := c.suspendAfter(); d < 0 {
		p = append(p, fmt.Sprintf("workbench.fleet.suspendAfter %s is negative", d))
	} else if d > 0 && len(c.Atespaces) == 0 {
		p = append(p, "workbench.fleet.suspendAfter needs workbench.fleet.atespaces, the Tasks that the front can suspend")
	}
	if c.wakeOnQueue() && (len(c.Atespaces) == 0 || c.Intake == "") {
		p = append(p, "workbench.fleet.wakeOnQueue needs workbench.fleet.atespaces, the Tasks that the front can wake, and workbench.fleet.intake, the queues that it reads")
	}
	return p
}
