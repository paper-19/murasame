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
)

var OutcomeMachine = &Machine[OutcomeStatus, OutcomeStatus]{
	Name:    "outcome",
	Initial: OutcomePending,
	Final: []OutcomeStatus{OutcomeMerged, OutcomeSetAside, OutcomeSkipped, OutcomeStopped, OutcomeNotTried,
		OutcomeInterrupted},
	Edges: map[Edge[OutcomeStatus, OutcomeStatus]]OutcomeStatus{
		{OutcomePending, OutcomeMerged}:      OutcomeMerged,
		{OutcomePending, OutcomeSetAside}:    OutcomeSetAside,
		{OutcomePending, OutcomeSkipped}:     OutcomeSkipped,
		{OutcomePending, OutcomeStopped}:     OutcomeStopped,
		{OutcomePending, OutcomeNotTried}:    OutcomeNotTried,
		{OutcomePending, OutcomeInterrupted}: OutcomeInterrupted,
	},
}

func OutcomeOf(status string) OutcomeStatus {
	if status == "" {
		return OutcomePending
	}
	return OutcomeStatus(status)
}
