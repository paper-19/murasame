package states

type Phase string

const (
	PhaseStarting Phase = "starting"
	PhaseSyncing  Phase = "syncing"
	PhaseBatch    Phase = "batch"
	PhaseIdle     Phase = "idle"
)

type PhaseEvent string

const (
	PhaseSync PhaseEvent = "sync"
	PhaseRun  PhaseEvent = "run_batch"
	PhaseRest PhaseEvent = "rest"
)

var PhaseMachine = &Machine[Phase, PhaseEvent]{
	Name:    "phase",
	Initial: PhaseStarting,
	Edges: map[Edge[Phase, PhaseEvent]]Phase{
		{PhaseStarting, PhaseSync}: PhaseSyncing,
		{PhaseIdle, PhaseSync}:     PhaseSyncing,
		{PhaseSyncing, PhaseRun}:   PhaseBatch,
		{PhaseSyncing, PhaseRest}:  PhaseIdle,
		{PhaseBatch, PhaseRest}:    PhaseIdle,
	},
}

func PhaseText(p Phase, what string) string {
	if what != "" {
		return string(p) + " (" + what + ")"
	}
	return string(p)
}
