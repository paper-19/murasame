package states

type Phase string

const (
	PhaseStarting Phase = "starting"
	PhaseSyncing  Phase = "syncing"
	PhaseBatch    Phase = "batch"
	PhaseGateWait Phase = "gate_wait"
	PhaseInLine   Phase = "waiting for a gate slot"
	PhaseIdle     Phase = "idle"
)

type PhaseEvent string

const (
	PhaseSync     PhaseEvent = "sync"
	PhaseRun      PhaseEvent = "run_batch"
	PhaseWaitSlot PhaseEvent = "wait_slot"
	PhaseGotSlot  PhaseEvent = "slot_answered"
	PhaseRest     PhaseEvent = "rest"
	PhaseQueue    PhaseEvent = "join_line"
)

var PhaseMachine = &Machine[Phase, PhaseEvent]{
	Name:    "phase",
	Initial: PhaseStarting,
	Edges: map[Edge[Phase, PhaseEvent]]Phase{
		{PhaseStarting, PhaseSync}:    PhaseSyncing,
		{PhaseIdle, PhaseSync}:        PhaseSyncing,
		{PhaseInLine, PhaseSync}:      PhaseSyncing,
		{PhaseSyncing, PhaseRun}:      PhaseBatch,
		{PhaseBatch, PhaseWaitSlot}:   PhaseGateWait,
		{PhaseGateWait, PhaseGotSlot}: PhaseBatch,
		{PhaseSyncing, PhaseRest}:     PhaseIdle,
		{PhaseSyncing, PhaseQueue}:    PhaseInLine,
		{PhaseBatch, PhaseRest}:       PhaseIdle,
		{PhaseBatch, PhaseQueue}:      PhaseInLine,
		{PhaseGateWait, PhaseRest}:    PhaseIdle,
		{PhaseGateWait, PhaseQueue}:   PhaseInLine,
	},
}

func PhaseText(p Phase, what string) string {
	if p == PhaseGateWait {
		return "waiting for a gate slot (" + what + ")"
	}
	return string(p)
}
