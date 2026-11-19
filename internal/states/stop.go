package states

type BatchStop string

const (
	BatchRunning   BatchStop = "running"
	BatchCancelled BatchStop = "cancelled"
	BatchStopping  BatchStop = "stopping"
	BatchRequeued  BatchStop = "requeued"
	BatchFinished  BatchStop = "finished"
)

type BatchStopEvent string

const (
	StopCancel  BatchStopEvent = "cancel"
	StopSignal  BatchStopEvent = "signal"
	StopRequeue BatchStopEvent = "requeue"
	StopFinish  BatchStopEvent = "finish"
)

var StopMachine = &Machine[BatchStop, BatchStopEvent]{
	Name:    "batch stop",
	Initial: BatchRunning,
	Final:   []BatchStop{BatchRequeued, BatchFinished},
	Edges: map[Edge[BatchStop, BatchStopEvent]]BatchStop{
		{BatchRunning, StopCancel}:    BatchCancelled,
		{BatchRunning, StopSignal}:    BatchStopping,
		{BatchRunning, StopFinish}:    BatchFinished,
		{BatchCancelled, StopCancel}:  BatchCancelled,
		{BatchCancelled, StopSignal}:  BatchCancelled,
		{BatchStopping, StopSignal}:   BatchStopping,
		{BatchStopping, StopCancel}:   BatchCancelled,
		{BatchCancelled, StopRequeue}: BatchRequeued,
		{BatchStopping, StopRequeue}:  BatchRequeued,
	},
}
