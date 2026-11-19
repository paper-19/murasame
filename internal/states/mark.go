package states

type Mark string

const (
	MarkNone      Mark = "none"
	MarkPending   Mark = "pending"
	MarkDone      Mark = "done"
	MarkDropped   Mark = "dropped"
	MarkAbandoned Mark = "abandoned"
)

type MarkEvent string

const (
	MarkLanded    MarkEvent = "landed"
	MarkMarked    MarkEvent = "marked"
	MarkSeen      MarkEvent = "seen_merged"
	MarkForgeErr  MarkEvent = "forge_error"
	MarkGone      MarkEvent = "gone"
	MarkGiveUp    MarkEvent = "give_up"
	MarkHeadMoved MarkEvent = "head_moved"
)

var MarkMachine = &Machine[Mark, MarkEvent]{
	Name:    "mark merged",
	Initial: MarkNone,
	Final:   []Mark{MarkDone, MarkDropped, MarkAbandoned},
	Edges: map[Edge[Mark, MarkEvent]]Mark{
		{MarkNone, MarkLanded}:       MarkPending,
		{MarkPending, MarkMarked}:    MarkDone,
		{MarkPending, MarkSeen}:      MarkDone,
		{MarkPending, MarkForgeErr}:  MarkPending,
		{MarkPending, MarkGone}:      MarkDropped,
		{MarkPending, MarkGiveUp}:    MarkDropped,
		{MarkPending, MarkHeadMoved}: MarkAbandoned,
	},
}
