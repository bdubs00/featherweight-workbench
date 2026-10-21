package sources

// This file reads the configuration of the sources: the list sources under
// the top-level key workbench of fw.yaml, which the package teams claims.
// No variable sets the list. A configuration that cannot work stops the
// start, so a source never runs open by accident.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Defaults and limits of a source.
const (
	// DefaultDedupFor is how long a key blocks a second run, 7 days.
	DefaultDedupFor = 7 * 24 * time.Hour
	// MinDedupFor is the shortest window that the configuration takes.
	MinDedupFor = time.Minute
	// DefaultEvery is the time between two polls.
	DefaultEvery = time.Minute
	// MinEvery is the shortest time between two polls.
	MinEvery = 5 * time.Second
	// DefaultPollTimeout bounds one poll.
	DefaultPollTimeout = 30 * time.Second
	// DefaultSignatureHeader carries the HMAC signature of a body.
	DefaultSignatureHeader = "X-Signature-256"
	// MaxKeyBytes is the longest de-duplication key.
	MaxKeyBytes = 512
	// DefaultKind is the kind of the principal of the runs of a source.
	DefaultKind = fw.KindWorkload
	// DefaultRedriveAttempts is the number of runs of an event with
	// redrive, the first one included.
	DefaultRedriveAttempts = 3
	// MinRedriveAttempts and MaxRedriveAttempts bound redrive.maxAttempts.
	MinRedriveAttempts, MaxRedriveAttempts = 2, 10
	// DefaultRedriveCooldown is the time from a dead letter to the next
	// run of the event.
	DefaultRedriveCooldown = 5 * time.Minute
	// MinRedriveCooldown is the shortest cooldown that the configuration
	// takes.
	MinRedriveCooldown = time.Minute
)

// Modes of the authentication of a webhook source.
const (
	AuthBearer = "bearer" // Authorization: Bearer <secret>
	AuthHMAC   = "hmac"   // the HMAC-SHA256 of the body, keyed with the secret, in a header
	AuthNone   = "none"   // no credential, only when the configuration says so
)

// Keys of the metadata that a source gives each run, beside fw.batch and
// fw.title.
const (
	MetaSource     = "workbench.source"           // the name of the source
	MetaKey        = "workbench.sourceKey"        // the de-duplication key of the event
	MetaGeneration = "workbench.sourceGeneration" // the generation of the claim, with redrive
)

// Source is one entry of workbench.sources. A source with Poll reads an
// HTTP endpoint at an interval. Any other source is a webhook that serves
// POST /v1/sources/<name>.
//
// Agent names the profile, and so the queue "<workbench.fleet.intake>.<agent>".
// Items is a dotted path to the array of events in the body or in the
// answer of the poll; empty means the body itself, one object or an array
// of objects. Input, Title, Metadata, and Key are Go templates over one
// event (render.go). Key is required. DedupFor is how long a key blocks a
// second run. RunAs names the principal of the runs, "source:<name>" by
// default, and RunAsKind its kind, workload by default or user for a
// person.
type Source struct {
	Name      string            `yaml:"name"`
	Agent     string            `yaml:"agent"`
	Auth      Auth              `yaml:"auth"`
	Poll      *Poll             `yaml:"poll"`
	Items     string            `yaml:"items"`
	Input     string            `yaml:"input"`
	Title     string            `yaml:"title"`
	Metadata  map[string]string `yaml:"metadata"`
	Key       string            `yaml:"key"`
	DedupFor  time.Duration     `yaml:"dedupFor"`
	Redrive   *Redrive          `yaml:"redrive"`
	RunAs     string            `yaml:"runAs"`
	RunAsKind string            `yaml:"runAsKind"`
}

// Redrive lets an event whose job reached the dead letter run again
// (redrive.go). After the cooldown, the next delivery of the event makes a
// new run, until the event made MaxAttempts runs. MaxAttempts counts the
// runs that the source started, the first one included, and not the
// retries of the kernel intake. The cooldown starts when the front reads
// the record of the dead letter. Redrive needs the bus nats.
type Redrive struct {
	MaxAttempts int           `yaml:"maxAttempts"`
	Cooldown    time.Duration `yaml:"cooldown"`
}

// Auth is how the caller of a webhook source proves itself. Mode is
// bearer, hmac, or none. Secret names the variable that holds the token
// or the key of the HMAC. Header names the header of the signature, for
// hmac only.
type Auth struct {
	Mode   string `yaml:"mode"`
	Secret string `yaml:"secret"`
	Header string `yaml:"header"`
}

// Poll is how a source reads an endpoint. URL is the endpoint, which
// answers JSON to a GET. Every is the time between two polls, and Timeout
// bounds one poll. Secret names the variable that holds a credential.
// Without Header, the credential goes as "Authorization: Bearer <value>";
// with Header, it goes as is in that header.
type Poll struct {
	URL     string        `yaml:"url"`
	Every   time.Duration `yaml:"every"`
	Timeout time.Duration `yaml:"timeout"`
	Secret  string        `yaml:"secret"`
	Header  string        `yaml:"header"`
}

// Kind returns "poll" or "webhook".
func (s Source) Kind() string {
	if s.Poll != nil {
		return "poll"
	}
	return "webhook"
}

// block is the part of the workbench block that this package reads.
type block struct {
	Sources []Source `yaml:"sources"`
}

// namePattern is the form of a source name: it is a path segment, a batch
// name, and a part of a bucket name.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// envPattern is the form of the name of a variable.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// headerPattern is the form of the name of a header.
var headerPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Load reads the sources from the block, fills the defaults, and checks
// them. It returns an error that names the key of a value that does not
// fit. A configuration without sources returns none and no error.
func Load(cfg *fw.Config) ([]Source, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(fleet.Block, &b); err != nil {
			return nil, err
		}
	}
	if len(b.Sources) == 0 {
		return nil, nil
	}
	// A source puts its runs on the intake queues of the workers.
	if _, err := fleet.Queue(cfg, "x"); err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	var problems []string
	seen := map[string]bool{}
	out := make([]Source, 0, len(b.Sources))
	for i, s := range b.Sources {
		s = withDefaults(s)
		at := fmt.Sprintf("workbench.sources[%d]", i)
		if s.Name != "" {
			at = "workbench.sources." + s.Name
		}
		if seen[s.Name] {
			problems = append(problems, fmt.Sprintf("%s: the name %q is used twice", at, s.Name))
		}
		seen[s.Name] = true
		problems = append(problems, check(cfg, at, s)...)
		out = append(out, s)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("sources: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

// withDefaults fills the defaults of one source.
func withDefaults(s Source) Source {
	s.Name, s.Agent = strings.TrimSpace(s.Name), strings.TrimSpace(s.Agent)
	s.Auth.Mode = strings.ToLower(strings.TrimSpace(s.Auth.Mode))
	s.Auth.Secret, s.Auth.Header = strings.TrimSpace(s.Auth.Secret), strings.TrimSpace(s.Auth.Header)
	if s.Auth.Mode == AuthHMAC && s.Auth.Header == "" {
		s.Auth.Header = DefaultSignatureHeader
	}
	if s.DedupFor == 0 {
		s.DedupFor = DefaultDedupFor
	}
	if s.Redrive != nil {
		r := *s.Redrive
		if r.MaxAttempts == 0 {
			r.MaxAttempts = DefaultRedriveAttempts
		}
		if r.Cooldown == 0 {
			r.Cooldown = DefaultRedriveCooldown
		}
		s.Redrive = &r
	}
	s.RunAs, s.RunAsKind = strings.TrimSpace(s.RunAs), strings.TrimSpace(s.RunAsKind)
	if s.RunAs == "" {
		s.RunAs = "source:" + s.Name
	}
	if s.RunAsKind == "" {
		s.RunAsKind = DefaultKind
	}
	if s.Poll != nil {
		p := *s.Poll
		p.URL, p.Secret, p.Header = strings.TrimSpace(p.URL), strings.TrimSpace(p.Secret), strings.TrimSpace(p.Header)
		if p.Every == 0 {
			p.Every = DefaultEvery
		}
		if p.Timeout == 0 {
			p.Timeout = DefaultPollTimeout
		}
		s.Poll = &p
	}
	return s
}

// check lists the problems of one source.
func check(cfg *fw.Config, at string, s Source) []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, at+": "+fmt.Sprintf(format, args...)) }
	if !namePattern.MatchString(s.Name) {
		add("the name %q must be 1 to 63 lower-case letters, digits, '-', or '_', and start with a letter or a digit", s.Name)
	}
	if s.Agent == "" {
		add("agent is required: it names the profile, and so the queue")
	} else if _, err := fleet.Queue(cfg, s.Agent); err != nil {
		add("%s", strings.TrimPrefix(err.Error(), "fw: invalid request: "))
	}
	if strings.TrimSpace(s.Key) == "" {
		add("key is required: it is the template of the de-duplication key of an event")
	}
	if _, err := compileTemplates(s); err != nil {
		add("%s", err)
	}
	for k := range s.Metadata {
		switch {
		case k == fw.MetaBatch || k == fw.MetaTitle || k == MetaSource || k == MetaKey || k == MetaGeneration:
			add("metadata.%s is set by the source", k)
		default:
			if err := fw.CheckMetadata(map[string]string{k: "x"}); err != nil {
				add("metadata.%s: %s", k, strings.TrimPrefix(err.Error(), "fw: invalid request: "))
			}
		}
	}
	if s.DedupFor < MinDedupFor {
		add("dedupFor %s is shorter than %s", s.DedupFor, MinDedupFor)
	}
	if r := s.Redrive; r != nil {
		if cfg == nil || cfg.Bus.Driver != "nats" {
			add("redrive needs bus.driver nats, which keeps the claims and the dead letter")
		}
		if r.MaxAttempts < MinRedriveAttempts || r.MaxAttempts > MaxRedriveAttempts {
			add("redrive.maxAttempts %d is not %d to %d; it counts the first run too", r.MaxAttempts, MinRedriveAttempts, MaxRedriveAttempts)
		}
		switch {
		case r.Cooldown < MinRedriveCooldown:
			add("redrive.cooldown %s is shorter than %s", r.Cooldown, MinRedriveCooldown)
		case r.Cooldown >= s.DedupFor:
			// The window of a key starts again at each write of its claim,
			// so a key never expires during its cooldown.
			add("redrive.cooldown %s is not shorter than dedupFor %s", r.Cooldown, s.DedupFor)
		}
	}
	if s.RunAsKind != fw.KindWorkload && s.RunAsKind != fw.KindUser {
		add("runAsKind %q is not %s or %s", s.RunAsKind, fw.KindWorkload, fw.KindUser)
	}
	if s.Poll != nil {
		p = append(p, checkPoll(at, s)...)
		return p
	}
	switch s.Auth.Mode {
	case AuthBearer, AuthHMAC:
		if !envPattern.MatchString(s.Auth.Secret) {
			add("auth.secret %q must name the variable that holds the %s secret", s.Auth.Secret, s.Auth.Mode)
		}
		if s.Auth.Mode == AuthHMAC && !headerPattern.MatchString(s.Auth.Header) {
			add("auth.header %q is not a header name", s.Auth.Header)
		}
	case AuthNone:
		if s.Auth.Secret != "" {
			add("auth.secret is set, but auth.mode is none")
		}
	case "":
		add("auth.mode is required: bearer, hmac, or none; a webhook without a credential must say none")
	default:
		add("auth.mode %q is not bearer, hmac, or none", s.Auth.Mode)
	}
	return p
}

// checkPoll lists the problems of the poll of a source.
func checkPoll(at string, s Source) []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, at+": "+fmt.Sprintf(format, args...)) }
	if s.Auth != (Auth{}) {
		add("auth applies to a webhook; a poll source has no route, so set poll.secret instead")
	}
	u, err := url.Parse(s.Poll.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("poll.url must be an absolute http or https URL")
	} else if u.User != nil {
		add("poll.url must not hold a user or a password; set poll.secret")
	}
	if s.Poll.Every < MinEvery {
		add("poll.every %s is shorter than %s", s.Poll.Every, MinEvery)
	}
	if s.Poll.Timeout < 0 {
		add("poll.timeout %s is negative", s.Poll.Timeout)
	}
	if s.Poll.Secret != "" && !envPattern.MatchString(s.Poll.Secret) {
		add("poll.secret %q must name a variable", s.Poll.Secret)
	}
	if s.Poll.Header != "" && (!headerPattern.MatchString(s.Poll.Header) || s.Poll.Secret == "") {
		add("poll.header %q must be a header name, and it needs poll.secret", s.Poll.Header)
	}
	return p
}
