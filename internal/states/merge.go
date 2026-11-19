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
	MergeHooks       MergeState = "hooks"
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
	MergeOnHooks     MergeEvent = "hooks"
	MergeOnDone      MergeEvent = "done"
	MergeOnFail      MergeEvent = "fail"
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
		{MergeResolving, MergeOnFail}:     MergeAside,
		{MergeResolving, MergeOnInfra}:    MergeInfra,
		{MergeResolved, MergeOnCommit}:    MergeCommitted,
		{MergeClean, MergeOnHooks}:        MergeHooks,
		{MergeCommitted, MergeOnHooks}:    MergeHooks,
		{MergeHooks, MergeOnDone}:         MergeDone,
		{MergeHooks, MergeOnFail}:         MergeAside,
		{MergeHooks, MergeOnInfra}:        MergeInfra,
	},
}

func (s MergeState) Merged() bool { return s == MergeClean || s == MergeCommitted || s == MergeDone }
