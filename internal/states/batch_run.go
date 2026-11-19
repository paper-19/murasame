package states

type BatchRunState string

type BatchRunEvent string

const (
	BatchRunRunning     BatchRunState = "running"
	BatchRunFinished    BatchRunState = "finished"
	BatchRunInterrupted BatchRunState = "interrupted"
)

const (
	BatchRunFinish    BatchRunEvent = "finish"
	BatchRunInterrupt BatchRunEvent = "interrupt"
)

var BatchRunMachine = &Machine[BatchRunState, BatchRunEvent]{
	Name:    "queue run",
	Initial: BatchRunRunning,
	Final:   []BatchRunState{BatchRunFinished, BatchRunInterrupted},
	Edges: map[Edge[BatchRunState, BatchRunEvent]]BatchRunState{
		{BatchRunRunning, BatchRunFinish}:    BatchRunFinished,
		{BatchRunRunning, BatchRunInterrupt}: BatchRunInterrupted,
	},
}

func BatchRunOf(finished, hasCode bool) BatchRunState {
	switch {
	case !finished:
		return BatchRunRunning
	case hasCode:
		return BatchRunFinished
	}
	return BatchRunInterrupted
}
