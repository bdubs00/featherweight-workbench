package bossmode

// This file serves the import of skills by an admin (docs/ARCHITECTURE.md
// section 20.7): the marketplaces of the configuration, the preview of a
// source, the install into the skills directory of the front, and the
// removal of an imported skill, as routes and as the MCP tools
// skill_preview and skill_install. An installed skill is instructions that every agent of a
// profile that allows it reads, so only an admin of the process imports,
// and the preview shows what the agents will load before an install.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the MCP tools of the import.
const (
	ToolSkillPreview = "skill_preview"
	ToolSkillInstall = "skill_install"
)

// InstallRequest is the body of POST /v1/bossmode/skills: a source,
// whether the install takes the place of an imported skill of that name,
// and the digest of the preview that the admin read, which a URL and a
// skill of a marketplace need (skills.Importer.Install).
type InstallRequest struct {
	skills.Source
	Replace bool   `json:"replace,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// Marketplaces is the answer of GET /v1/bossmode/marketplaces.
type Marketplaces struct {
	Marketplaces []skills.Index `json:"marketplaces"`
}

// Installed is the answer of POST /v1/bossmode/skills: the entry of the
// catalog and the preview of what the install wrote.
type Installed struct {
	Skill    skills.Entry   `json:"skill"`
	Replaced bool           `json:"replaced"`
	Preview  skills.Preview `json:"preview"`
}

// mountImports mounts the routes of the import.
func mountImports(k *native.Kit) {
	a := k.Agent()
	k.Handle("GET /v1/bossmode/marketplaces", func(w http.ResponseWriter, r *http.Request) {
		im, ok := importerOf(w, r, a)
		if !ok {
			return
		}
		if err := mayImport(r.Context(), a); err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, Marketplaces{Marketplaces: im.Market().List(r.Context())})
	})
	k.Handle("POST /v1/bossmode/skills/preview", func(w http.ResponseWriter, r *http.Request) {
		im, ok := importerOf(w, r, a)
		if !ok {
			return
		}
		if err := mayImport(r.Context(), a); err != nil {
			k.Fail(w, r, err)
			return
		}
		var src skills.Source
		if !k.DecodeFiles(w, r, &src, func() int { return len(src.Files) }) {
			return
		}
		p, err := im.Preview(r.Context(), src)
		if err != nil {
			failImport(k, w, r, err, nil)
			return
		}
		native.WriteJSON(w, http.StatusOK, p)
	})
	k.Handle("POST /v1/bossmode/skills", func(w http.ResponseWriter, r *http.Request) {
		im, ok := importerOf(w, r, a)
		if !ok {
			return
		}
		if err := mayImport(r.Context(), a); err != nil {
			k.Fail(w, r, err)
			return
		}
		var req InstallRequest
		if !k.DecodeFiles(w, r, &req, func() int { return len(req.Files) }) {
			return
		}
		e, p, err := im.Install(r.Context(), req.Source, req.Replace, req.Digest, subjectOf(r.Context()))
		if err != nil {
			failImport(k, w, r, err, &p)
			return
		}
		status := http.StatusCreated
		if p.Exists != nil {
			status = http.StatusOK
		}
		w.Header().Set("Location", "/v1/bossmode/skills")
		native.WriteJSON(w, status, Installed{Skill: e, Replaced: p.Exists != nil, Preview: p})
	})
	k.Handle("DELETE /v1/bossmode/skills/{name}", func(w http.ResponseWriter, r *http.Request) {
		im, ok := importerOf(w, r, a)
		if !ok {
			return
		}
		if err := mayImport(r.Context(), a); err != nil {
			k.Fail(w, r, err)
			return
		}
		if err := im.Remove(r.PathValue("name"), subjectOf(r.Context())); err != nil {
			failImport(k, w, r, err, nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// importerOf returns the importer of the hub, or writes the problem.
func importerOf(w http.ResponseWriter, r *http.Request, a *fw.Agent) (*skills.Importer, bool) {
	h := Of(a)
	if h == nil || h.imp == nil {
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "bossmode has not started in this process")
		return nil, false
	}
	return h.imp, true
}

// mayImport decides whether the caller in ctx imports skills: the admins
// of the process (access.admins), and the process itself.
func mayImport(ctx context.Context, a *fw.Agent) error {
	err := a.Authorize(ctx, fw.ActionAdmin, fw.Target{})
	if errors.Is(err, fw.ErrForbidden) {
		return fmt.Errorf("%w: an import of a skill needs the admin role of the process (access.admins), because every agent whose profile allows the skill reads it", fw.ErrForbidden)
	}
	return err
}

// importStatus is the status of an error of an import.
func importStatus(err error, p *skills.Preview) int {
	var fe *skills.FetchError
	switch {
	case errors.As(err, &fe):
		return fe.Status
	case errors.Is(err, skills.ErrExists), errors.Is(err, skills.ErrNotImported), errors.Is(err, skills.ErrChanged):
		return http.StatusConflict
	case errors.Is(err, skills.ErrNotFoundSkill):
		return http.StatusNotFound
	case errors.Is(err, skills.ErrRefused):
		if p != nil {
			for _, pr := range p.Problems {
				if pr.Blocks && (pr.Code == skills.ProblemTooLarge || pr.Code == skills.ProblemTooManyFiles) {
					return http.StatusRequestEntityTooLarge
				}
			}
		}
		return http.StatusBadRequest
	case errors.Is(err, fw.ErrInvalidRequest):
		return http.StatusBadRequest
	}
	return 0
}

// failImport answers an error of an import with its status.
func failImport(k *native.Kit, w http.ResponseWriter, r *http.Request, err error, p *skills.Preview) {
	if status := importStatus(err, p); status != 0 {
		native.WriteProblem(w, r, status, importDetail(err))
		return
	}
	k.Fail(w, r, err)
}

// importDetail is the words of an error of an import.
func importDetail(err error) string {
	s := err.Error()
	for _, prefix := range []string{skills.ErrExists.Error() + ": ", skills.ErrRefused.Error() + ": ", skills.ErrNotImported.Error() + ": ", skills.ErrNotFoundSkill.Error() + ": ", skills.ErrChanged.Error() + ": ", "fw: invalid request: ", "skills: "} {
		s = strings.TrimPrefix(s, prefix)
	}
	return s
}

// importArgs are the arguments of skill_preview and skill_install.
type importArgs struct {
	URL         string        `json:"url,omitempty"`
	Marketplace string        `json:"marketplace,omitempty"`
	Name        string        `json:"name,omitempty"`
	Text        string        `json:"text,omitempty"`
	Files       []skills.File `json:"files,omitempty"`
	Replace     bool          `json:"replace,omitempty"`
	Digest      string        `json:"digest,omitempty"`
}

func (in importArgs) source() skills.Source {
	return skills.Source{URL: in.URL, Marketplace: in.Marketplace, Name: in.Name, Text: in.Text, Files: in.Files}
}

const importProps = `
	"url":{"type":"string","description":"A SKILL.md, the directory of a skill in a repository, or a zip archive."},
	"marketplace":{"type":"string","description":"A marketplace of the configuration, with name."},
	"name":{"type":"string","description":"With marketplace: the skill of the marketplace."},
	"text":{"type":"string","description":"A SKILL.md as text."},
	"files":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path","text"]},"description":"The text files of a skill, each with its path in the skill directory."}`

// importTools are the tools of the import.
func importTools(a *fw.Agent) []mcp.Tool {
	yes, no := true, false
	refuseRun := func(ctx context.Context) *sdk.CallToolResult {
		if _, ok := mcp.CallRun(ctx); ok {
			return mcp.ErrorResult("A run of the fleet imports no skill. An admin imports a skill through the page of bossmode, the routes, or this tool from a client of the admin.")
		}
		if err := mayImport(ctx, a); err != nil {
			return mcp.ErrorResult(mcp.Explain(err))
		}
		if h := Of(a); h == nil || h.imp == nil {
			return mcp.ErrorResult("Bossmode has not started in this process.")
		}
		return nil
	}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolSkillPreview,
			Title:       "Preview a skill",
			Description: "Fetch a skill from a url, a marketplace with name, a text, or files, and check it, for an admin. It installs nothing. The answer holds the front matter, the body, the files with their sizes, the digest, and each problem. An installed skill is instructions that every agent of a profile that allows it reads, so read the preview before an install.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` + importProps + `}}`),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &yes},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in importArgs) (*sdk.CallToolResult, any, error) {
			if res := refuseRun(ctx); res != nil {
				return res, nil, nil
			}
			p, err := Of(a).imp.Preview(ctx, in.source())
			if err != nil {
				return mcp.ErrorResult(importWords(err)), nil, nil
			}
			return mcp.Result(previewText(p), p), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolSkillInstall,
			Title:       "Install a skill",
			Description: "Install a skill from a url, a marketplace with name, a text, or files into the skills of the server, for an admin, after the checks of skill_preview. An installed skill is instructions for every agent whose profile allows it, so read the preview first. A name that an imported skill holds needs replace true; a skill of the configuration is never replaced.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` + importProps + `,
	"replace":{"type":"boolean","description":"Take the place of an imported skill of the same name."},
	"digest":{"type":"string","description":"The digest of the preview that you read, from skill_preview. A url and a marketplace need it, and the install refuses files that differ from the preview."}}}`),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &no},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in importArgs) (*sdk.CallToolResult, any, error) {
			if res := refuseRun(ctx); res != nil {
				return res, nil, nil
			}
			e, p, err := Of(a).imp.Install(ctx, in.source(), in.Replace, in.Digest, subjectOf(ctx))
			if err != nil {
				return mcp.ErrorResult(importWords(err)), nil, nil
			}
			verb := "Installed"
			if p.Exists != nil {
				verb = "Replaced"
			}
			text := fmt.Sprintf("%s the skill %s (%s) under the imported skills of the server. A person can hand it to every agent whose profile allows it with a slash command from now on.", verb, e.Name, p.Digest)
			return mcp.Result(text, Installed{Skill: e, Replaced: p.Exists != nil, Preview: p}), nil, nil
		}),
	}
}

// importWords words an error of an import for a tool result.
func importWords(err error) string {
	switch importStatus(err, nil) {
	case http.StatusNotFound:
		return "Not found: " + importDetail(err) + "."
	case http.StatusConflict:
		return "Conflict: " + importDetail(err) + "."
	case http.StatusRequestEntityTooLarge:
		return "Too large: " + importDetail(err) + "."
	case http.StatusBadRequest:
		return "Invalid request: " + importDetail(err) + "."
	case http.StatusBadGateway:
		return "The host did not give the skill: " + importDetail(err) + "."
	}
	return mcp.Explain(err)
}

// previewText is a preview as text for a model.
func previewText(p skills.Preview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Preview of the skill %s", p.Name)
	if p.Description != "" {
		b.WriteString(": " + p.Description)
	}
	fmt.Fprintf(&b, "\nDigest %s, %d files:", p.Digest, len(p.Files))
	for _, f := range p.Files {
		fmt.Fprintf(&b, " %s (%d bytes)", f.Path, f.Size)
	}
	if len(p.Problems) == 0 {
		b.WriteString("\nNo problem.")
	}
	for _, pr := range p.Problems {
		mark := "note"
		if pr.Blocks {
			mark = "blocks"
		}
		fmt.Fprintf(&b, "\n- %s (%s): %s", pr.Code, mark, pr.Text)
	}
	if p.Installable {
		b.WriteString("\nAn install can write it.")
	} else {
		b.WriteString("\nAn install refuses it as it is.")
	}
	b.WriteString("\nThe body of SKILL.md, which every agent that loads the skill reads:\n" + p.Body)
	return b.String()
}
