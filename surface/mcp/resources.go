package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resource URIs.
const (
	runPrefix   = "fw://runs/"
	skillPrefix = "fw://skills/"
	ledgerPart  = "/ledger"
)

// addResources adds the run, ledger, file, and skill templates, and one
// resource per skill that exists now.
func (s *server) addResources() {
	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: runPrefix + "{id}",
		Name:        "run",
		Title:       "Run",
		Description: "The run object: state, input, output, pending approval, and usage.",
		MIMEType:    "application/json",
	}, s.readRun)
	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: runPrefix + "{id}" + ledgerPart,
		Name:        "ledger",
		Title:       "Run ledger",
		Description: "Every event of the run, one CloudEvents JSON object per line.",
		MIMEType:    "application/jsonl",
	}, s.readLedger)
	// {+path} keeps the slashes of a path, so fw://runs/{id}/files/docs/a.md
	// matches as well as docs%2Fa.md.
	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: runPrefix + "{id}" + filesPart + "{+path}",
		Name:        "run-file",
		Title:       "Run file",
		Description: "A file that a run changed, as an item of kind file in run_events names it: text for a UTF-8 text file, else a blob with its media type, up to 4 MiB. The reader rules of run_file apply. It reads the runs of this process only: for a run of another fleet member, use run_file with raw true.",
	}, s.readFile)
	// {+name} keeps the slash of the name of a skill of an MCP server, so
	// fw://skills/github/review matches as well as github%2Freview.
	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: skillPrefix + "{+name}",
		Name:        "skill",
		Title:       "Skill",
		Description: "The SKILL.md file of a skill of the agent, as GET /v1/skills lists it: a skill of the skills directories of the configuration (tools.skills, or skills under the home directory), or a skill of an MCP server under <server>/<name>, which the kernel reads from the server and checks at the moment of use.",
		MIMEType:    "text/markdown",
	}, s.readSkill)
	for _, sk := range s.skills() {
		s.srv.AddResource(&sdk.Resource{
			URI:         skillURI(sk.Name),
			Name:        sk.Name,
			Title:       "Skill " + sk.Name,
			Description: truncate(skillDescription(sk), 300),
			MIMEType:    "text/markdown",
		}, s.readSkill)
	}
}

// runFromURI returns the run id of fw://runs/{id} or fw://runs/{id}/ledger.
func runFromURI(uri, suffix string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, runPrefix)
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, suffix)
	if !ok && suffix != "" {
		return "", false
	}
	id, err := url.PathUnescape(rest)
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func (s *server) readRun(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	id, ok := runFromURI(uri, "")
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	r, err := s.a.Get(ctx, id)
	if err != nil {
		if errors.Is(err, fw.ErrRunNotFound) {
			return nil, sdk.ResourceNotFoundError(uri)
		}
		return nil, fmt.Errorf("mcp: read %s: %w", uri, err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: encode run %s: %w", id, err)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(b)}}}, nil
}

func (s *server) readLedger(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	id, ok := runFromURI(uri, ledgerPart)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	if _, err := s.a.Get(ctx, id); err != nil {
		if errors.Is(err, fw.ErrRunNotFound) {
			return nil, sdk.ResourceNotFoundError(uri)
		}
		return nil, fmt.Errorf("mcp: read %s: %w", uri, err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for e, err := range s.a.Ledger().Read(ctx, id, 0) {
		if err != nil {
			return nil, fmt.Errorf("mcp: read ledger of run %s: %w", id, err)
		}
		if err := enc.Encode(e); err != nil {
			return nil, fmt.Errorf("mcp: encode event of run %s: %w", id, err)
		}
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "application/jsonl", Text: buf.String()}}}, nil
}

// skills lists the skills of the agent as the kernel serves them
// (fw.Agent.Skills): the skills of the skill directories, then the skills
// of each MCP server under <server>/<name>. The kernel logs a read error
// and keeps the skills that it could read.
func (s *server) skills() []fw.Skill {
	return s.a.Skills()
}

// skillURI is the URI of the resource of a skill. Each part of the name is
// escaped on its own, so the name of a skill of an MCP server keeps its
// slash: fw://skills/github/review.
func skillURI(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return skillPrefix + strings.Join(parts, "/")
}

// skillFromURI returns the name of the skill of fw://skills/{name}. It
// decodes the escaped name, so github%2Freview and github/review name the
// same skill of a server. A name with an empty part, a part "." or "..",
// or a control character is no name.
func skillFromURI(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, skillPrefix)
	if !ok {
		return "", false
	}
	name, err := url.PathUnescape(rest)
	if err != nil || name == "" {
		return "", false
	}
	for _, p := range strings.Split(name, "/") {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return name, true
}

// skillDescription is the description of a skill, or a line that names it.
func skillDescription(sk fw.Skill) string {
	if sk.Description != "" {
		return sk.Description
	}
	return "The skill " + sk.Name + "."
}

// readSkill reads SKILL.md of a skill of the agent. A skill of an MCP
// server is read from the server at the moment of use, and the kernel
// checks it against the listing, as the skill tool does.
func (s *server) readSkill(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	name, ok := skillFromURI(uri)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	sk, ok := s.a.SkillNamed(name)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	body, err := sk.ReadFile(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("mcp: read skill %s: %w", name, err)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: body}}}, nil
}
