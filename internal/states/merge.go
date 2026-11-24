package states

type MergeState string

type MergeEvent string

const (
	MergeMerging     MergeState = "merging"
	MergeClean       MergeState = "clean"
	MergeConflicted  MergeState = "conflicted"
	MergeRefused     MergeState = "refused"
	MergeResolving   MergeState = "resolving"
	MergeResolved    MergeState = "resolved"
	MergeCommitted   MergeState = "committed"
	MergeDone        MergeState = "done"
	MergeAside       MergeState = "aside"
	MergeNeedsPerson MergeState = "needs_person"
	MergeInfra       MergeState = "infra"
)

const (
	MergeOnClean     MergeEvent = "clean"
	MergeOnConflict  MergeEvent = "conflict"
	MergeOnRefuse    MergeEvent = "refuse"
	MergeOnUnrelated MergeEvent = "unrelated"
	MergeOnResolve   MergeEvent = "resolve"
	MergeOnResolved  MergeEvent = "resolved"
	MergeOnPerson    MergeEvent = "needs_person"
	MergeOnCommit    MergeEvent = "commit"
	MergeOnDone      MergeEvent = "done"
	MergeOnInfra     MergeEvent = "infra"
)

var MergeMachine = &Machine[MergeState, MergeEvent]{
	Name:    "branch merge",
	Initial: MergeMerging,
	Final:   []MergeState{MergeDone, MergeAside, MergeNeedsPerson, MergeInfra},
	Edges: map[Edge[MergeState, MergeEvent]]MergeState{
		{MergeMerging, MergeOnClean}:      MergeClean,
		{MergeMerging, MergeOnConflict}:   MergeConflicted,
		{MergeMerging, MergeOnRefuse}:     MergeRefused,
		{MergeRefused, MergeOnUnrelated}:  MergeAside,
		{MergeRefused, MergeOnInfra}:      MergeInfra,
		{MergeConflicted, MergeOnResolve}: MergeResolving,
		{MergeResolving, MergeOnResolved}: MergeResolved,
		{MergeResolving, MergeOnPerson}:   MergeNeedsPerson,
		{MergeResolving, MergeOnInfra}:    MergeInfra,
		{MergeResolved, MergeOnCommit}:    MergeCommitted,
		{MergeClean, MergeOnDone}:         MergeDone,
		{MergeCommitted, MergeOnDone}:     MergeDone,
	},
}

func (s MergeState) Merged() bool { return s == MergeClean || s == MergeCommitted || s == MergeDone }
