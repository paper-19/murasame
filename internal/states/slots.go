package states

type Slot string

const (
	SlotOut     Slot = "out"
	SlotWaiting Slot = "waiting"
	SlotHolding Slot = "holding"
)

type SlotEvent string

const (
	SlotJoin     SlotEvent = "join"
	SlotGrant    SlotEvent = "grant"
	SlotRelease  SlotEvent = "release"
	SlotWithdraw SlotEvent = "withdraw"
)

var SlotMachine = &Machine[Slot, SlotEvent]{
	Name:    "gate slot line",
	Initial: SlotOut,
	Edges: map[Edge[Slot, SlotEvent]]Slot{
		{SlotOut, SlotJoin}:         SlotWaiting,
		{SlotWaiting, SlotGrant}:    SlotHolding,
		{SlotHolding, SlotRelease}:  SlotOut,
		{SlotWaiting, SlotWithdraw}: SlotOut,
	},
}
