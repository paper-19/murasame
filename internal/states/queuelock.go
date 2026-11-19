package states

type Lock string

const (
	LockFree     Lock = "free"
	LockWaiting  Lock = "waiting"
	LockHeld     Lock = "held"
	LockReleased Lock = "released"
	LockTimedOut Lock = "timed_out"
	LockRefused  Lock = "refused"
)

type LockEvent string

const (
	LockTake    LockEvent = "take"
	LockBusy    LockEvent = "busy"
	LockExpire  LockEvent = "expire"
	LockRelease LockEvent = "release"
	LockRefuse  LockEvent = "refuse"
	LockRetry   LockEvent = "retry"
)

var LockMachine = &Machine[Lock, LockEvent]{
	Name:    "queue lock",
	Initial: LockFree,
	Final:   []Lock{LockReleased},
	Edges: map[Edge[Lock, LockEvent]]Lock{
		{LockFree, LockTake}:   LockHeld,
		{LockFree, LockBusy}:   LockWaiting,
		{LockFree, LockRefuse}: LockRefused,

		{LockWaiting, LockTake}:   LockHeld,
		{LockWaiting, LockExpire}: LockTimedOut,
		{LockWaiting, LockRefuse}: LockRefused,

		{LockHeld, LockRelease}: LockReleased,
		{LockHeld, LockRefuse}:  LockRefused,

		{LockTimedOut, LockRetry}: LockFree,
		{LockRefused, LockRetry}:  LockFree,
	},
}
