package manager

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bridges answers whether the operator of a bridge delegated an action to
// the manager. batch is the fw.batch of a run, bridge:<id>, and action is
// one of DelegateApprove, DelegateBudget, DelegateMembers, and
// DelegateReport. The package bridges keeps the delegation flags in the
// record of each bridge, and its service sets its store with SetBridges.
type Bridges interface {
	Delegates(ctx context.Context, batch, action string) (bool, error)
}

// The actions that the operator of a bridge delegates to the manager, as
// the delegation flags of the record of a bridge name them.
const (
	DelegateApprove = "approve" // decide the approvals of the members
	DelegateBudget  = "budget"  // change the budgets of the members
	DelegateMembers = "members" // add, remove, and cancel the members
	DelegateReport  = "report"  // write the report of the bridge
)

// BridgePrefix starts the batch name of every run of a bridge,
// bridge:<id>. The package bridges owns the batch names.
const BridgePrefix = "bridge:"

// delegation holds the Bridges of the process. It is the one value of the
// package that changes after init: the service of the package bridges sets
// it when it starts and clears it when it stops, and the doctor line reads
// it.
var delegation struct {
	mu sync.RWMutex
	b  Bridges
}

// SetBridges sets the Bridges that Delegated asks. A nil value, also a nil
// pointer in an interface, delegates nothing, so every action of a manager
// on a run of a bridge is refused.
func SetBridges(b Bridges) {
	if b != nil {
		if v := reflect.ValueOf(b); (v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Func) && v.IsNil() {
			b = nil
		}
	}
	delegation.mu.Lock()
	defer delegation.mu.Unlock()
	delegation.b = b
}

// ClearBridges removes b when it is the Bridges that SetBridges set last,
// so the store of an agent that stops leaves no Bridges behind, and the
// store of a newer agent stays. The package bridges calls SetBridges when
// its service starts and ClearBridges when it stops.
func ClearBridges(b Bridges) {
	if b == nil || !reflect.TypeOf(b).Comparable() {
		return
	}
	delegation.mu.Lock()
	defer delegation.mu.Unlock()
	if delegation.b == b {
		delegation.b = nil
	}
}

// currentBridges returns the Bridges that SetBridges set, or nil.
func currentBridges() Bridges {
	delegation.mu.RLock()
	defer delegation.mu.RUnlock()
	return delegation.b
}

// NotDelegated is the refusal of an action of the manager on a run of a
// bridge whose operator did not delegate it. It is fw.ErrForbidden.
type NotDelegated struct {
	Batch  string
	Action string
}

func (e *NotDelegated) Error() string {
	return fmt.Sprintf("%v: the operator did not delegate %s for this bridge (%s)", fw.ErrForbidden, delegationWords(e.Action), e.Batch)
}

// Unwrap makes the refusal fw.ErrForbidden.
func (e *NotDelegated) Unwrap() error { return fw.ErrForbidden }

// delegationWords names an action of a bridge in a sentence.
func delegationWords(action string) string {
	switch action {
	case DelegateApprove:
		return "approvals"
	case DelegateBudget:
		return "budget changes"
	case DelegateMembers:
		return "changes to the members"
	case DelegateReport:
		return "the report"
	}
	return action
}

// Delegated decides whether the actor can do the action on a run of the
// batch. It returns nil when the actor is not a run of the manager
// (Actor.IsManager), because the policy of its profile alone binds such a
// run, and when the batch is not the batch of a bridge, because the policy
// of the manager alone binds the manager outside a bridge. On a run of a
// bridge, the manager needs the delegation of the operator: Delegated asks
// Bridges and returns a *NotDelegated when it says no or when no Bridges
// is set. A Bridges that fails gives its error.
func Delegated(ctx context.Context, cfg *fw.Config, act Actor, batch, action string) error {
	if !act.IsManager(cfg) || !strings.HasPrefix(batch, BridgePrefix) {
		return nil
	}
	b := currentBridges()
	if b == nil {
		return &NotDelegated{Batch: batch, Action: action}
	}
	ok, err := b.Delegates(ctx, batch, action)
	if err != nil {
		return fmt.Errorf("manager: the bridges did not say whether the operator of %s delegated %s: %w", batch, delegationWords(action), err)
	}
	if !ok {
		return &NotDelegated{Batch: batch, Action: action}
	}
	return nil
}
