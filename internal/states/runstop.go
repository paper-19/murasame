package states

type RunStop string

const (
	RunStarting     RunStop = "starting"
	RunUnlocked     RunStop = "unlocked"
	RunLocked       RunStop = "locked"
	RunSignalled    RunStop = "signalled"
	RunFinished     RunStop = "finished"
	RunDone         RunStop = "done"
	RunAbandoned    RunStop = "abandoned"
	RunSecondSignal RunStop = "second_signal"
)

type RunStopEvent string

const (
	RunBegins  RunStopEvent = "begins"
	RunLocks   RunStopEvent = "locks"
	RunUnlocks RunStopEvent = "unlocks"
	RunSignal  RunStopEvent = "signal"
	RunReturns RunStopEvent = "returns"
	RunStops   RunStopEvent = "stops"
)

var RunStopMachine = &Machine[RunStop, RunStopEvent]{
	Name:    "run interruption",
	Initial: RunStarting,
	Final:   []RunStop{RunDone, RunAbandoned, RunSecondSignal},
	Edges: map[Edge[RunStop, RunStopEvent]]RunStop{
		{RunStarting, RunBegins}:  RunUnlocked,
		{RunStarting, RunSignal}:  RunAbandoned,
		{RunStarting, RunReturns}: RunFinished,

		{RunUnlocked, RunLocks}:   RunLocked,
		{RunUnlocked, RunSignal}:  RunAbandoned,
		{RunUnlocked, RunReturns}: RunFinished,

		{RunLocked, RunSignal}:  RunSignalled,
		{RunLocked, RunUnlocks}: RunFinished,
		{RunLocked, RunReturns}: RunFinished,

		{RunSignalled, RunSignal}:  RunSecondSignal,
		{RunSignalled, RunUnlocks}: RunSignalled,
		{RunSignalled, RunReturns}: RunFinished,

		{RunFinished, RunSignal}:  RunFinished,
		{RunFinished, RunReturns}: RunFinished,
		{RunFinished, RunStops}:   RunDone,
	},
}
