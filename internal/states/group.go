package states

type GroupState string

type GroupEvent string

const (
	GroupPending       GroupState = "pending"
	GroupBuilding      GroupState = "building"
	GroupEmpty         GroupState = "empty"
	GroupBuilt         GroupState = "built"
	GroupKnownFail     GroupState = "known_fail"
	GroupRecordPass    GroupState = "record_pass"
	GroupGating        GroupState = "gating"
	GroupPassed        GroupState = "passed"
	GroupFailed        GroupState = "failed"
	GroupInfra         GroupState = "infra"
	GroupLanding       GroupState = "landing"
	GroupLanded        GroupState = "landed"
	GroupStopped       GroupState = "stopped"
	GroupBisected      GroupState = "bisected"
	GroupPairCheck     GroupState = "pair_check"
	GroupAside         GroupState = "aside"
	GroupAsideAlone    GroupState = "aside_alone"
	GroupAsideTogether GroupState = "aside_together"
)

const (
	GroupOnBuild    GroupEvent = "build"
	GroupOnNothing  GroupEvent = "nothing"
	GroupOnBuilt    GroupEvent = "built"
	GroupOnKnown    GroupEvent = "known"
	GroupOnRecord   GroupEvent = "record"
	GroupOnGate     GroupEvent = "gate"
	GroupOnPass     GroupEvent = "pass"
	GroupOnFail     GroupEvent = "fail"
	GroupOnInfra    GroupEvent = "infra"
	GroupOnLand     GroupEvent = "land"
	GroupOnLanded   GroupEvent = "landed"
	GroupOnStop     GroupEvent = "stop"
	GroupOnSplit    GroupEvent = "split"
	GroupOnPair     GroupEvent = "pair"
	GroupOnAside    GroupEvent = "aside"
	GroupOnAlone    GroupEvent = "alone"
	GroupOnTogether GroupEvent = "together"
)

var GroupMachine = &Machine[GroupState, GroupEvent]{
	Name:    "group",
	Initial: GroupPending,
	Final: []GroupState{GroupEmpty, GroupLanded, GroupStopped, GroupBisected, GroupAside, GroupAsideAlone,
		GroupAsideTogether},
	Edges: map[Edge[GroupState, GroupEvent]]GroupState{
		{GroupPending, GroupOnBuild}:      GroupBuilding,
		{GroupBuilding, GroupOnNothing}:   GroupEmpty,
		{GroupBuilding, GroupOnBuilt}:     GroupBuilt,
		{GroupBuilding, GroupOnStop}:      GroupStopped,
		{GroupBuilt, GroupOnKnown}:        GroupKnownFail,
		{GroupBuilt, GroupOnRecord}:       GroupRecordPass,
		{GroupBuilt, GroupOnGate}:         GroupGating,
		{GroupGating, GroupOnPass}:        GroupPassed,
		{GroupGating, GroupOnFail}:        GroupFailed,
		{GroupGating, GroupOnInfra}:       GroupInfra,
		{GroupInfra, GroupOnStop}:         GroupStopped,
		{GroupRecordPass, GroupOnLand}:    GroupLanding,
		{GroupPassed, GroupOnLand}:        GroupLanding,
		{GroupLanding, GroupOnLanded}:     GroupLanded,
		{GroupLanding, GroupOnStop}:       GroupStopped,
		{GroupFailed, GroupOnSplit}:       GroupBisected,
		{GroupFailed, GroupOnAside}:       GroupAside,
		{GroupFailed, GroupOnStop}:        GroupStopped,
		{GroupKnownFail, GroupOnSplit}:    GroupBisected,
		{GroupKnownFail, GroupOnAside}:    GroupAside,
		{GroupKnownFail, GroupOnPair}:     GroupPairCheck,
		{GroupPairCheck, GroupOnAlone}:    GroupAsideAlone,
		{GroupPairCheck, GroupOnTogether}: GroupAsideTogether,
		{GroupPairCheck, GroupOnAside}:    GroupAside,
		{GroupPairCheck, GroupOnStop}:     GroupStopped,
	},
}
