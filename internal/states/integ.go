package states

type IntegState string

type IntegEvent string

const (
	IntegUnknown  IntegState = "unknown"
	IntegAtTarget IntegState = "at_target"
	IntegBuilding IntegState = "building"
	IntegBuilt    IntegState = "built"
	IntegGated    IntegState = "gated"
	IntegLanded   IntegState = "landed"
	IntegDirty    IntegState = "dirty"
)

const (
	IntegOnReset   IntegEvent = "reset"
	IntegOnBuild   IntegEvent = "build"
	IntegOnNothing IntegEvent = "nothing"
	IntegOnBuilt   IntegEvent = "built"
	IntegOnGate    IntegEvent = "gate"
	IntegOnLand    IntegEvent = "land"
	IntegOnDirty   IntegEvent = "dirty"
)

var IntegMachine = &Machine[IntegState, IntegEvent]{
	Name:    "integ",
	Initial: IntegUnknown,
	Edges: map[Edge[IntegState, IntegEvent]]IntegState{
		{IntegUnknown, IntegOnReset}:    IntegAtTarget,
		{IntegAtTarget, IntegOnReset}:   IntegAtTarget,
		{IntegAtTarget, IntegOnBuild}:   IntegBuilding,
		{IntegBuilding, IntegOnNothing}: IntegAtTarget,
		{IntegBuilding, IntegOnBuilt}:   IntegBuilt,
		{IntegBuilding, IntegOnReset}:   IntegAtTarget,
		{IntegBuilding, IntegOnDirty}:   IntegDirty,
		{IntegBuilt, IntegOnGate}:       IntegGated,
		{IntegBuilt, IntegOnReset}:      IntegAtTarget,
		{IntegBuilt, IntegOnDirty}:      IntegDirty,
		{IntegGated, IntegOnLand}:       IntegLanded,
		{IntegGated, IntegOnReset}:      IntegAtTarget,
		{IntegGated, IntegOnDirty}:      IntegDirty,
		{IntegLanded, IntegOnReset}:     IntegAtTarget,
		{IntegLanded, IntegOnDirty}:     IntegDirty,
		{IntegDirty, IntegOnReset}:      IntegAtTarget,
	},
}

func (s IntegState) Working() bool {
	return s == IntegBuilding || s == IntegBuilt || s == IntegGated || s == IntegLanded
}
