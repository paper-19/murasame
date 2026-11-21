package states

type OutcomeStatus string

const (
	OutcomePending     OutcomeStatus = "pending"
	OutcomeMerged      OutcomeStatus = "merged"
	OutcomeSetAside    OutcomeStatus = "set_aside"
	OutcomeSkipped     OutcomeStatus = "skipped"
	OutcomeStopped     OutcomeStatus = "stopped"
	OutcomeNotTried    OutcomeStatus = "not_tried"
	OutcomeInterrupted OutcomeStatus = "interrupted"
	OutcomeRecorded    OutcomeStatus = "recorded"
)

var OutcomeMachine = &Machine[OutcomeStatus, OutcomeStatus]{
	Name:    "outcome",
	Initial: OutcomePending,
	Final: []OutcomeStatus{OutcomeMerged, OutcomeSetAside, OutcomeSkipped, OutcomeStopped, OutcomeNotTried,
		OutcomeInterrupted, OutcomeRecorded},
	Edges: map[Edge[OutcomeStatus, OutcomeStatus]]OutcomeStatus{
		{OutcomePending, OutcomeMerged}:      OutcomeMerged,
		{OutcomePending, OutcomeSetAside}:    OutcomeSetAside,
		{OutcomePending, OutcomeSkipped}:     OutcomeSkipped,
		{OutcomePending, OutcomeStopped}:     OutcomeStopped,
		{OutcomePending, OutcomeNotTried}:    OutcomeNotTried,
		{OutcomePending, OutcomeInterrupted}: OutcomeInterrupted,
		{OutcomePending, OutcomeRecorded}:    OutcomeRecorded,
	},
}

func OutcomeOf(status string) OutcomeStatus {
	if status == "" {
		return OutcomePending
	}
	return OutcomeStatus(status)
}
