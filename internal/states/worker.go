package states

type Worker string

const (
	WorkerNew          Worker = "new"
	WorkerWoken        Worker = "woken"
	WorkerTicking      Worker = "ticking"
	WorkerTickingWoken Worker = "ticking_woken"
	WorkerIdle         Worker = "idle"
	WorkerStopping     Worker = "stopping"
	WorkerJoined       Worker = "joined"
)

type WorkerEvent string

const (
	WorkerStart  WorkerEvent = "start"
	WorkerWake   WorkerEvent = "wake"
	WorkerTimer  WorkerEvent = "timer"
	WorkerBegin  WorkerEvent = "begin"
	WorkerTicked WorkerEvent = "ticked"
	WorkerStop   WorkerEvent = "stop"
	WorkerExit   WorkerEvent = "exit"
)

var WorkerMachine = &Machine[Worker, WorkerEvent]{
	Name:    "worker",
	Initial: WorkerNew,
	Final:   []Worker{WorkerJoined},
	Edges: map[Edge[Worker, WorkerEvent]]Worker{
		{WorkerNew, WorkerStart}: WorkerWoken,
		{WorkerNew, WorkerWake}:  WorkerNew,
		{WorkerNew, WorkerStop}:  WorkerJoined,

		{WorkerWoken, WorkerBegin}: WorkerTicking,
		{WorkerWoken, WorkerWake}:  WorkerWoken,
		{WorkerWoken, WorkerTimer}: WorkerWoken,
		{WorkerWoken, WorkerStop}:  WorkerStopping,

		{WorkerTicking, WorkerWake}:   WorkerTickingWoken,
		{WorkerTicking, WorkerTicked}: WorkerIdle,
		{WorkerTicking, WorkerStop}:   WorkerStopping,

		{WorkerTickingWoken, WorkerWake}:   WorkerTickingWoken,
		{WorkerTickingWoken, WorkerTicked}: WorkerWoken,
		{WorkerTickingWoken, WorkerStop}:   WorkerStopping,

		{WorkerIdle, WorkerWake}:  WorkerWoken,
		{WorkerIdle, WorkerTimer}: WorkerWoken,
		{WorkerIdle, WorkerStop}:  WorkerStopping,

		{WorkerStopping, WorkerWake}:   WorkerStopping,
		{WorkerStopping, WorkerTimer}:  WorkerStopping,
		{WorkerStopping, WorkerTicked}: WorkerStopping,
		{WorkerStopping, WorkerStop}:   WorkerStopping,
		{WorkerStopping, WorkerExit}:   WorkerJoined,
	},
}

type Runtime string

const (
	RuntimeCreated  Runtime = "created"
	RuntimeRunning  Runtime = "running"
	RuntimeStopping Runtime = "stopping"
	RuntimeJoined   Runtime = "joined"
)

type RuntimeEvent string

const (
	RuntimeStart RuntimeEvent = "start"
	RuntimeStop  RuntimeEvent = "stop"
	RuntimeJoin  RuntimeEvent = "join"
)

var RuntimeMachine = &Machine[Runtime, RuntimeEvent]{
	Name:    "runtime",
	Initial: RuntimeCreated,
	Final:   []Runtime{RuntimeJoined},
	Edges: map[Edge[Runtime, RuntimeEvent]]Runtime{
		{RuntimeCreated, RuntimeStart}: RuntimeRunning,
		{RuntimeCreated, RuntimeStop}:  RuntimeStopping,
		{RuntimeRunning, RuntimeStop}:  RuntimeStopping,
		{RuntimeStopping, RuntimeStop}: RuntimeStopping,
		{RuntimeStopping, RuntimeJoin}: RuntimeJoined,
	},
}
