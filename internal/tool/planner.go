package tool

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// PreviewTTL is how long a preview can be claimed by a confirm:true call.
const PreviewTTL = 15 * time.Minute

// PlanCapacity is the number of pending previews a [Planner] keeps.
const PlanCapacity = 32

// Planner records the previews of [ConfirmByFlag] tools in memory (one per
// session) so an agent-only confirm can prove a preview of the same call
// came first. Previews are bound to the tool name and [CanonicalArgs]; they
// expire after [PreviewTTL], at most [PlanCapacity] are kept (oldest evicted
// first) and each is claimed once. A nil *Planner records nothing and every
// Claim fails, so a confirm without a plan store fails closed.
type Planner struct {
	mu    sync.Mutex
	now   func() time.Time
	plans []plan // oldest first
}

type plan struct {
	tool    string
	args    [sha256.Size]byte
	expires time.Time
}

// NewPlanner returns an empty Planner reading time from now (nil means
// time.Now).
func NewPlanner(now func() time.Time) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{now: now}
}

// Propose records a preview of toolName with canonical args.
func (p *Planner) Propose(toolName string, args []byte) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.plans = append(p.prune(now), plan{tool: toolName, args: sha256.Sum256(args), expires: now.Add(PreviewTTL)})
	if n := len(p.plans) - PlanCapacity; n > 0 {
		p.plans = p.plans[n:]
	}
}

// Claim consumes the newest unexpired preview of toolName with the same
// canonical args. Failures wrap [ErrPreviewRequired].
func (p *Planner) Claim(toolName string, args []byte) error {
	if p != nil {
		h := sha256.Sum256(args)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.plans = p.prune(p.now())
		for i := len(p.plans) - 1; i >= 0; i-- {
			if pl := p.plans[i]; pl.tool == toolName && pl.args == h {
				p.plans = append(p.plans[:i], p.plans[i+1:]...)
				return nil
			}
		}
	}
	return fmt.Errorf("%w: no matching preview for %s", ErrPreviewRequired, toolName)
}

// prune drops expired plans; they are ordered by expiry.
func (p *Planner) prune(now time.Time) []plan {
	i := 0
	for i < len(p.plans) && !now.Before(p.plans[i].expires) {
		i++
	}
	return p.plans[i:]
}
