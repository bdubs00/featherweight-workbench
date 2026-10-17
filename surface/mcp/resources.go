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
	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: skillPrefix + "{name}",
		Name:        "skill",
		Title:       "Skill",
		Description: "The SKILL.md file of a skill from the skills directories of the configuration (tools.skills, or skills under the home directory).",
		MIMEType:    "text/markdown",
	}, s.readSkill)
	for _, sk := range s.skills() {
		s.srv.AddResource(&sdk.Resource{
			URI:         skillPrefix + url.PathEscape(sk.Name),
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

// skills lists the skills of the configured skill directories, as the
// kernel sees them. A read error is logged and leaves the skills that
// could be read.
func (s *server) skills() []fw.Skill {
	skills, err := fw.LoadSkills(s.a.Config().SkillDirs()...)
	if err != nil {
		s.log.Warn("mcp: load skills", "error", err)
	}
	return skills
}

// skillDescription is the description of a skill, or a line that names it.
func skillDescription(sk fw.Skill) string {
	if sk.Description != "" {
		return sk.Description
	}
	return "The skill " + sk.Name + "."
}

func (s *server) readSkill(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	rest, ok := strings.CutPrefix(uri, skillPrefix)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	name, err := url.PathUnescape(rest)
	if err != nil || name == "" || fw.ValidSkillName(name) != name {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	sk, ok := s.a.SkillNamed(name)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	body, err := sk.Body()
	if err != nil {
		return nil, fmt.Errorf("mcp: read skill %s: %w", name, err)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: body}}}, nil
}
