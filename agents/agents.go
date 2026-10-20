// Package agents creates, changes, and removes agent profiles for people.
// The kernel reads its profiles from the configuration and runs them. It
// writes none, so this package does: it checks a profile with the kernel,
// authorizes the caller, writes the file under <home>/agents through the
// store of the config package, and hands the kernel the new set with
// fw.Agent.SetProfiles, so a run can name the profile at once. See
// docs/ARCHITECTURE.md section 10.
package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// Store keeps the profile files. The config package provides the store
// that writes one YAML file per profile under <home>/agents.
type Store interface {
	Put(ctx context.Context, p fw.AgentProfile) error
	Delete(ctx context.Context, name string) error
}

// OpenStore opens the store of a configuration. A test replaces it with a
// store in memory.
var OpenStore = func(cfg *fw.Config) (Store, error) { return config.OpenProfileStore(cfg) }

// Draft is the view of a profile that is not saved: what the agent would
// do, and whether saving it replaces an agent that exists.
type Draft struct {
	view.AgentView
	Exists bool `json:"exists"`
}

// Place keeps profiles. Local is the store of this process. The package
// fleet gives the place of a profile that a pool of the fleet runs, so
// Put, Delete, and Preview change that pool for each of its workers
// (docs/ARCHITECTURE.md section 14.9).
//
// Profile returns the profile of that name in the place. Check runs the
// checks of the kernel on a profile that the caller wants to save there.
// Authorize decides an action of the kernel, such as fw.ActionAgentEdit,
// on a profile of the place. Locked reports whether the configuration
// defines the profile, so no write can change it. Save writes a profile
// and makes it live, and Remove removes one. View returns a saved profile
// as the caller sees it, Role the role of the caller on a profile of that
// name, and Abilities what a profile can do in plain words.
type Place interface {
	Profile(name string) (fw.AgentProfile, bool)
	Check(p fw.AgentProfile) error
	Authorize(ctx context.Context, action, name string) error
	Locked(name string) bool
	Save(ctx context.Context, p fw.AgentProfile) error
	Remove(ctx context.Context, name string) error
	View(ctx context.Context, name string) (view.AgentView, error)
	Role(ctx context.Context, name string) string
	Abilities(ctx context.Context, p fw.AgentProfile) ([]view.Ability, error)
}

// Put creates or replaces a profile for the caller and returns the saved
// view and whether the profile is new. It normalizes the profile first
// (see Normalize) and checks it with the kernel. A new profile needs
// fw.ActionAgentCreate, and a change needs fw.ActionAgentEdit. A profile
// that the configuration defines cannot change. A new profile without an
// access block gets the caller as its owner, so an individual or a group
// owns the agents that they make. A replaced profile that names no access
// block keeps the access block it had, so an edit never drops the owners
// by accident.
func Put(ctx context.Context, a *fw.Agent, p fw.AgentProfile) (view.AgentView, bool, error) {
	return PutIn(ctx, a, Local(a), p)
}

// PutIn is Put for a profile of the place at.
func PutIn(ctx context.Context, a *fw.Agent, at Place, p fw.AgentProfile) (view.AgentView, bool, error) {
	exists, err := check(ctx, at, &p)
	if err != nil {
		return view.AgentView{}, false, err
	}
	p = owned(ctx, at, p, exists)
	if err := at.Save(ctx, p); err != nil {
		return view.AgentView{}, false, err
	}
	a.Logger().Info("profile saved", "profile", p.Name, "created", !exists, "principal", subjectOf(ctx))
	v, err := at.View(ctx, p.Name)
	return v, !exists, err
}

// owned applies the access rules of Put: a replaced profile without an
// access block keeps the block it had, and a new one gets the caller as
// its owner.
func owned(ctx context.Context, at Place, p fw.AgentProfile, exists bool) fw.AgentProfile {
	if p.Access != nil {
		return p
	}
	if exists {
		old, _ := at.Profile(p.Name)
		p.Access = old.Access
		return p
	}
	if principal := fw.PrincipalFrom(ctx); principal != nil && principal.Kind != fw.KindAnonymous {
		p.Access = &fw.Access{Owners: []string{principal.Subject}}
	}
	return p
}

// Delete removes a profile. It needs fw.ActionAgentDelete, and it refuses a
// profile that the configuration defines. An unknown name is
// fw.ErrProfileNotFound. The runs of the profile stay in the ledger.
func Delete(ctx context.Context, a *fw.Agent, name string) error {
	return DeleteIn(ctx, a, Local(a), name)
}

// DeleteIn is Delete for a profile of the place at.
func DeleteIn(ctx context.Context, a *fw.Agent, at Place, name string) error {
	name = strings.TrimSpace(name)
	if _, ok := at.Profile(name); !ok {
		return fmt.Errorf("%w: %s", fw.ErrProfileNotFound, name)
	}
	if err := at.Authorize(ctx, fw.ActionAgentDelete, name); err != nil {
		return err
	}
	if at.Locked(name) {
		return errLocked(name)
	}
	if err := at.Remove(ctx, name); err != nil {
		return err
	}
	a.Logger().Info("profile deleted", "profile", name, "principal", subjectOf(ctx))
	return nil
}

// Preview checks a profile as Put does and changes nothing. A new profile
// shows the role owner, which the caller gets when the profile names no
// access.
func Preview(ctx context.Context, a *fw.Agent, p fw.AgentProfile) (Draft, error) {
	return PreviewIn(ctx, Local(a), p)
}

// PreviewIn is Preview for a profile of the place at.
func PreviewIn(ctx context.Context, at Place, p fw.AgentProfile) (Draft, error) {
	exists, err := check(ctx, at, &p)
	if err != nil {
		return Draft{}, err
	}
	d := Draft{AgentView: view.AgentView{AgentProfile: p, Role: "owner"}, Exists: exists}
	if exists {
		d.Role = at.Role(ctx, p.Name)
	}
	if d.Abilities, err = at.Abilities(ctx, p); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// check runs the checks of Put and Preview: the profile is normalized, the
// place accepts it with the access block that Put saves, the caller may
// create or edit it, and the configuration does not define it. It reports
// whether the profile exists.
func check(ctx context.Context, at Place, p *fw.AgentProfile) (bool, error) {
	if err := Normalize(p); err != nil {
		return false, err
	}
	_, exists := at.Profile(p.Name)
	if err := at.Check(owned(ctx, at, *p, exists)); err != nil {
		return false, err
	}
	action := fw.ActionAgentCreate
	if exists {
		action = fw.ActionAgentEdit
	}
	if err := at.Authorize(ctx, action, p.Name); err != nil {
		return false, err
	}
	if at.Locked(p.Name) {
		return false, errLocked(p.Name)
	}
	return exists, nil
}

// Local returns the place of the profiles of this process: the files under
// <home>/agents, which the store of the config package writes, and the set
// of the kernel, which fw.Agent.SetProfiles makes live.
func Local(a *fw.Agent) Place { return local{a} }

type local struct{ a *fw.Agent }

func (l local) Profile(name string) (fw.AgentProfile, bool) { return l.a.ProfileNamed(name) }
func (l local) Check(p fw.AgentProfile) error               { return l.a.CheckProfile(p) }
func (l local) Locked(name string) bool                     { return l.a.ProfileLocked(name) }

func (l local) Authorize(ctx context.Context, action, name string) error {
	return l.a.Authorize(ctx, action, fw.Target{Agent: name})
}

func (l local) Save(ctx context.Context, p fw.AgentProfile) error {
	store, err := OpenStore(l.a.Config())
	if err != nil {
		return err
	}
	if err := store.Put(ctx, p); err != nil {
		return fmt.Errorf("agents: store the profile %s: %w", p.Name, err)
	}
	next := l.a.ProfileSet()
	next[p.Name] = p
	return l.a.SetProfiles(next)
}

func (l local) Remove(ctx context.Context, name string) error {
	store, err := OpenStore(l.a.Config())
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, name); err != nil {
		return fmt.Errorf("agents: delete the profile %s: %w", name, err)
	}
	next := l.a.ProfileSet()
	delete(next, name)
	return l.a.SetProfiles(next)
}

func (l local) View(ctx context.Context, name string) (view.AgentView, error) {
	return view.Agent(ctx, l.a, name)
}

func (l local) Role(ctx context.Context, name string) string { return view.RoleOf(ctx, l.a, name) }

func (l local) Abilities(ctx context.Context, p fw.AgentProfile) ([]view.Ability, error) {
	return view.Abilities(ctx, l.a, p)
}

func errLocked(name string) error {
	return fmt.Errorf("%w: the configuration defines the agent %s, so change it there", fw.ErrInvalidRequest, name)
}

// Normalize trims the profile: the name, the effort, the time zone, the
// policy default, and the access entries. It refuses, with
// fw.ErrInvalidRequest, a time zone that fw.LoadZone does not resolve and a
// policy default that is not allow, ask, or deny. The kernel checks the
// rest.
func Normalize(p *fw.AgentProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Effort = strings.ToLower(strings.TrimSpace(p.Effort))
	p.Model = strings.TrimSpace(p.Model)
	p.TimeZone = strings.TrimSpace(p.TimeZone)
	if p.TimeZone != "" {
		if _, _, err := fw.LoadZone(p.TimeZone); err != nil {
			return fmt.Errorf("%w: timeZone %q is not an IANA time zone such as America/Chicago, or UTC", fw.ErrInvalidRequest, p.TimeZone)
		}
	}
	if p.Policy != nil {
		r := *p.Policy
		r.Default = strings.ToLower(strings.TrimSpace(r.Default))
		switch r.Default {
		case "", "allow", "ask", "deny":
		default:
			return fmt.Errorf("%w: policy.default %q is not allow, ask, or deny", fw.ErrInvalidRequest, p.Policy.Default)
		}
		p.Policy = &r
	}
	if p.Access != nil {
		acc := fw.Access{
			Owners:    cleanList(p.Access.Owners),
			Operators: cleanList(p.Access.Operators),
			Viewers:   cleanList(p.Access.Viewers),
		}
		p.Access = &acc
	}
	return nil
}

// cleanList trims the entries and drops the empty ones. An empty result is
// nil, so an access block with no entry stays empty in JSON and in YAML.
func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func subjectOf(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}
