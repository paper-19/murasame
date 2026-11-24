package service

import (
	"fmt"
	"sync"

	"murasame/internal/states"
	"murasame/internal/store"
)

func approvalOf(p *store.PR) states.Approval {
	if p.Approval != "" {
		return p.Approval
	}
	if p.ApprovedHead == "" && !inStates(p, states.Queued, states.Testing, states.SetAside, states.Held) {
		return states.NoApproval
	}
	if len(p.GateApproves) > 0 {
		return states.PendingReapprove
	}
	return states.Counted
}

func stepApproval(p *store.PR, evs ...states.ApprovalEvent) {
	at := approvalOf(p)
	for _, ev := range evs {
		at = states.ApprovalMachine.Must(at, ev)
	}
	p.Approval = at
}

func grant(p *store.PR, by string) {
	stepApproval(p, states.Approved)
	p.ApprovedBy, p.ApprovedHead, p.GateApproves = by, p.Head, nil
}

func outdate(p *store.PR) {
	if states.ApprovalMachine.Can(approvalOf(p), states.Pushed) {
		stepApproval(p, states.Pushed)
	}
	p.ApprovedHead = ""
}

func withdraw(p *store.PR) {
	stepApproval(p, states.Withdrawn)
	clearApproval(p)
}

func retarget(p *store.PR) {
	if states.ApprovalMachine.Can(approvalOf(p), states.Retargeted) {
		stepApproval(p, states.Retargeted)
	}
	clearApproval(p)
}

func duringBatch(p *store.PR, a store.GateApprove) {
	stepApproval(p, states.ApprovedDuringBatch)
	p.GateApproves = append(append([]store.GateApprove{}, p.GateApproves...), a)
}

func batchStarts(p *store.PR) {
	stepApproval(p, states.BatchStarted)
	p.GateApproves = nil
}

func markOf(p *store.PR) states.Mark {
	if p.MarkMerged != "" {
		return states.MarkPending
	}
	return states.MarkNone
}

func stepMark(p *store.PR, ev states.MarkEvent, sha string) states.Mark {
	to := states.MarkMachine.Must(markOf(p), ev)
	switch {
	case ev == states.MarkLanded:
		p.MarkMerged, p.MarkTries = sha, 0
	case ev == states.MarkForgeErr:
		p.MarkTries++
	case states.MarkMachine.IsFinal(to):
		p.MarkMerged, p.MarkTries = "", 0
	}
	return to
}

const markTriesWhenClosed = 5

type batchStop struct {
	mu        sync.Mutex
	t         *states.Tracker[states.BatchStop, states.BatchStopEvent]
	cancelWhy string
	stopWhy   string
}

type stopSnapshot struct {
	at                 states.BatchStop
	live               bool
	cancelWhy, stopWhy string
}

func (b *batchStop) begin(stopping string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.t, b.cancelWhy, b.stopWhy = states.StopMachine.Start(), "", ""
	if stopping != "" {
		b.fire(states.StopSignal)
		b.stopWhy = stopping
	}
}

func (b *batchStop) fire(ev states.BatchStopEvent) bool {
	if b.t == nil || !b.t.Can(ev) {
		return false
	}
	b.t.Must(ev)
	return true
}

func (b *batchStop) cancel(why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.state()
	if b.fire(states.StopCancel) && was != states.BatchCancelled {
		b.cancelWhy = why
	}
}

func (b *batchStop) signal(why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fire(states.StopSignal) && b.stopWhy == "" {
		b.stopWhy = why
	}
}

func (b *batchStop) state() states.BatchStop {
	if b.t == nil {
		return states.BatchFinished
	}
	return b.t.State()
}

func (b *batchStop) cancelled() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state() == states.BatchCancelled {
		return b.cancelWhy
	}
	return ""
}

func (b *batchStop) reason() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state() {
	case states.BatchCancelled:
		return b.cancelWhy
	case states.BatchStopping:
		return b.stopWhy
	}
	return ""
}

func (b *batchStop) end() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.fire(states.StopRequeue) {
		b.fire(states.StopFinish)
	}
	b.t = nil
}

func (b *batchStop) snapshot() stopSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return stopSnapshot{b.state(), b.t != nil, b.cancelWhy, b.stopWhy}
}

func (b *batchStop) restore(s stopSnapshot, stopping string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !s.live {
		b.t = nil
		return
	}
	b.t = states.StopMachine.At(s.at)
	b.cancelWhy, b.stopWhy = s.cancelWhy, s.stopWhy
	if stopping != "" && b.fire(states.StopSignal) && b.stopWhy == "" {
		b.stopWhy = stopping
	}
}

func backoffOf(d *store.ServiceData) states.Backoff {
	if d.BackoffUntil != nil {
		return states.BackingOff
	}
	return states.BackoffOK
}

func (s *Service) expireBackoff() {
	if backoffOf(s.Data) != states.BackingOff || s.Now() < s.Data.Until() {
		return
	}
	states.BackoffMachine.Must(states.BackingOff, states.BackoffExpire)
	check(s.store().ClearBackoff())
	s.Data.BackoffUntil, s.Data.BackoffWhy = nil, ""
}

func (s *Service) phase(ev states.PhaseEvent, what string) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	to, err := states.PhaseMachine.Step(s.live.Phase, ev)
	if err != nil {
		s.Say(fmt.Sprintf("status page: %v", err))
		return
	}
	s.live.Phase, s.live.What = to, what
	s.live.Gate = nil
}

func (l Live) PhaseText() string { return states.PhaseText(l.Phase, l.What) }
