package states

type Approval string

const (
	NoApproval       Approval = "none"
	Counted          Approval = "counted"
	PendingReapprove Approval = "pending_reapprove"
	StaleByPush      Approval = "stale_by_push"
)

type ApprovalEvent string

const (
	Approved            ApprovalEvent = "approve"
	ApprovedDuringBatch ApprovalEvent = "approve_during_batch"
	BatchStarted        ApprovalEvent = "batch_start"
	Pushed              ApprovalEvent = "push"
	Withdrawn           ApprovalEvent = "withdraw"
	Retargeted          ApprovalEvent = "retarget"
)

var ApprovalMachine = &Machine[Approval, ApprovalEvent]{
	Name:    "approval",
	Initial: NoApproval,
	Edges: map[Edge[Approval, ApprovalEvent]]Approval{
		{NoApproval, Approved}:                  Counted,
		{StaleByPush, Approved}:                 Counted,
		{Counted, Approved}:                     Counted,
		{PendingReapprove, Approved}:            Counted,
		{Counted, ApprovedDuringBatch}:          PendingReapprove,
		{PendingReapprove, ApprovedDuringBatch}: PendingReapprove,
		{Counted, BatchStarted}:                 Counted,
		{PendingReapprove, BatchStarted}:        Counted,
		{Counted, Pushed}:                       StaleByPush,
		{PendingReapprove, Pushed}:              StaleByPush,
		{StaleByPush, Pushed}:                   StaleByPush,
		{Counted, Withdrawn}:                    NoApproval,
		{PendingReapprove, Withdrawn}:           NoApproval,
		{Counted, Retargeted}:                   NoApproval,
		{PendingReapprove, Retargeted}:          NoApproval,
		{StaleByPush, Retargeted}:               NoApproval,
	},
}
