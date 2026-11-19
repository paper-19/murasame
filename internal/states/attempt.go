package states

import "strings"

type AttemptState string

type AttemptEvent string

const (
	AttemptStarting    AttemptState = "starting"
	AttemptStartFailed AttemptState = "start_failed"
	AttemptRunning     AttemptState = "running"
	AttemptExited      AttemptState = "exited"
	AttemptTimedOut    AttemptState = "timed_out"
	AttemptCancelled   AttemptState = "cancelled"
)

const (
	AttemptSpawnFailed AttemptEvent = "spawn_failed"
	AttemptSpawned     AttemptEvent = "spawned"
	AttemptExit        AttemptEvent = "exit"
	AttemptExecFailed  AttemptEvent = "exec_failed"
	AttemptDeadline    AttemptEvent = "deadline"
	AttemptCancel      AttemptEvent = "cancel"
	AttemptClean       AttemptEvent = "clean"
	AttemptSurvivors   AttemptEvent = "survivors"
	AttemptUnconfirmed AttemptEvent = "unconfirmed"
)

var AttemptOutcomes = []AttemptState{AttemptStartFailed, AttemptExited, AttemptTimedOut, AttemptCancelled}

var AttemptCleanups = []AttemptEvent{AttemptClean, AttemptSurvivors, AttemptUnconfirmed}

func AttemptEnd(outcome AttemptState, cleanup AttemptEvent) AttemptState {
	return AttemptState(string(outcome) + "/" + string(cleanup))
}

func AttemptOutcome(s AttemptState) AttemptState {
	outcome, _, ok := strings.Cut(string(s), "/")
	if !ok {
		return ""
	}
	return AttemptState(outcome)
}

func AttemptCleanup(s AttemptState) AttemptEvent {
	_, cleanup, ok := strings.Cut(string(s), "/")
	if !ok {
		return ""
	}
	return AttemptEvent(cleanup)
}

var AttemptMachine = func() *Machine[AttemptState, AttemptEvent] {
	m := &Machine[AttemptState, AttemptEvent]{
		Name:    "command attempt",
		Initial: AttemptStarting,
		Edges: map[Edge[AttemptState, AttemptEvent]]AttemptState{
			{AttemptStarting, AttemptSpawnFailed}: AttemptStartFailed,
			{AttemptStarting, AttemptSpawned}:     AttemptRunning,
			{AttemptRunning, AttemptExit}:         AttemptExited,
			{AttemptRunning, AttemptExecFailed}:   AttemptStartFailed,
			{AttemptRunning, AttemptDeadline}:     AttemptTimedOut,
			{AttemptRunning, AttemptCancel}:       AttemptCancelled,
		},
	}
	for _, o := range AttemptOutcomes {
		for _, c := range AttemptCleanups {
			end := AttemptEnd(o, c)
			m.Edges[Edge[AttemptState, AttemptEvent]{o, c}] = end
			m.Final = append(m.Final, end)
		}
	}
	return m
}()

type Verdict string

const (
	VerdictPass  Verdict = "pass"
	VerdictFail  Verdict = "fail"
	VerdictOK    Verdict = "ok"
	VerdictAside Verdict = "aside"
	VerdictInfra Verdict = "infra"
)

func GateVerdict(s AttemptState, code int) Verdict {
	if !AttemptMachine.IsFinal(s) || AttemptCleanup(s) != AttemptClean || AttemptOutcome(s) != AttemptExited {
		return VerdictInfra
	}
	switch code {
	case 0:
		return VerdictPass
	case 1:
		return VerdictFail
	}
	return VerdictInfra
}

func CommandVerdict(s AttemptState, code int) Verdict {
	if !AttemptMachine.IsFinal(s) || AttemptCleanup(s) != AttemptClean || AttemptOutcome(s) != AttemptExited {
		return VerdictInfra
	}
	switch {
	case code == 0:
		return VerdictOK
	case code > 0:
		return VerdictAside
	}
	return VerdictInfra
}
