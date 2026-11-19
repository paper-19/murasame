package states

type Backoff string

const (
	BackoffOK  Backoff = "ok"
	BackingOff Backoff = "backing_off"
)

type BackoffEvent string

const (
	BackoffInfra  BackoffEvent = "infra_stop"
	BackoffExpire BackoffEvent = "expire"
)

var BackoffMachine = &Machine[Backoff, BackoffEvent]{
	Name:    "backoff",
	Initial: BackoffOK,
	Edges: map[Edge[Backoff, BackoffEvent]]Backoff{
		{BackoffOK, BackoffInfra}:   BackingOff,
		{BackingOff, BackoffInfra}:  BackingOff,
		{BackingOff, BackoffExpire}: BackoffOK,
	},
}

func NewStopReason(key, last string) bool { return key != "" && key != last }
