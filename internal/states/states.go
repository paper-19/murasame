package states

import (
	"fmt"
	"time"
)

type State string

const (
	Open     State = "open"
	Queued   State = "queued"
	Testing  State = "testing"
	SetAside State = "set_aside"
	Held     State = "held"
	Merged   State = "merged"
	Closed   State = "closed"
)

type Event string

const (
	Approve         Event = "approve"
	Unapprove       Event = "unapprove"
	Cancel          Event = "cancel"
	Retry           Event = "retry"
	Push            Event = "push"
	Start           Event = "start"
	Land            Event = "land"
	SetAsideEv      Event = "set_aside"
	Requeue         Event = "requeue"
	Close           Event = "close"
	Reopen          Event = "reopen"
	MergedElsewhere Event = "merged_elsewhere"
	Recover         Event = "recover"
)

var PRMachine = &Machine[State, Event]{
	Name:    "pr",
	Initial: Open,
	Final:   []State{Merged},
	Edges: map[Edge[State, Event]]State{
		{Open, Approve}:             Queued,
		{SetAside, Approve}:         Queued,
		{SetAside, Retry}:           Queued,
		{Held, Retry}:               Queued,
		{Queued, Unapprove}:         Open,
		{Testing, Unapprove}:        Open,
		{SetAside, Unapprove}:       Open,
		{Held, Unapprove}:           Open,
		{Queued, Cancel}:            Held,
		{Testing, Cancel}:           Held,
		{Queued, Push}:              Open,
		{SetAside, Push}:            Open,
		{Held, Push}:                Open,
		{Testing, Push}:             Open,
		{Queued, Start}:             Testing,
		{Testing, Land}:             Merged,
		{Testing, SetAsideEv}:       SetAside,
		{Testing, Requeue}:          Queued,
		{Testing, Recover}:          Queued,
		{Open, Close}:               Closed,
		{Queued, Close}:             Closed,
		{Testing, Close}:            Closed,
		{SetAside, Close}:           Closed,
		{Held, Close}:               Closed,
		{Closed, Reopen}:            Open,
		{Open, MergedElsewhere}:     Merged,
		{Queued, MergedElsewhere}:   Merged,
		{SetAside, MergedElsewhere}: Merged,
		{Held, MergedElsewhere}:     Merged,
		{Testing, MergedElsewhere}:  Merged,
	},
}

var Edges = PRMachine.Edges

type InvalidTransition struct{ Msg string }

func (e *InvalidTransition) Error() string { return e.Msg }

func Can(state State, event Event) bool { return PRMachine.Can(state, event) }

type Move [5]string

func Transition(number int, old State, hist []Move, event Event, why string) (State, []Move, error) {
	if old == "" {
		old = Open
	}
	next, ok := PRMachine.Next(old, event)
	if !ok {
		return "", nil, &InvalidTransition{fmt.Sprintf("#%d: %s does not apply when %s", number, event, old)}
	}
	if len([]rune(why)) > 200 {
		why = string([]rune(why)[:200])
	}
	hist = append(append([]Move{}, hist...), Move{time.Now().Format("2006-01-02 15:04:05"), string(event), string(old), string(next), why})
	if len(hist) > 30 {
		hist = hist[len(hist)-30:]
	}
	return next, hist, nil
}
