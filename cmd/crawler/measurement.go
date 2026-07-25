package main

import (
	"strings"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
)

type measurementNetwork struct {
	Current               int            `json:"current"`
	ExecutionCurrent      int            `json:"execution_current"`
	ConsensusCurrent      int            `json:"consensus_current"`
	ExecutionStale        int            `json:"execution_stale"`
	ConsensusStale        int            `json:"consensus_stale"`
	MembershipVerified    int            `json:"membership_verified"`
	MembershipClaimed     int            `json:"membership_claimed"`
	FingerprintIdentified int            `json:"fingerprint_identified"`
	FingerprintDirection  map[string]int `json:"fingerprint_direction"`
}

type measurementDistinct struct {
	Walker      string  `json:"walker"`
	Protocol    string  `json:"protocol"`
	Family      string  `json:"family"`
	Estimate    uint64  `json:"estimate"`
	Sightings   uint64  `json:"sightings"`
	WindowStart string  `json:"window_start"`
	WindowEnd   string  `json:"window_end"`
	Error       float64 `json:"relative_error"`
}

type measurementPoint struct {
	GeneratedAt     string                        `json:"generated_at"`
	ForkEvaluatedAt string                        `json:"fork_evaluated_at"`
	SchemaVersion   int                           `json:"schema_version"`
	Methodology     string                        `json:"methodology_version"`
	Run             snapshot.RunMetadata          `json:"run"`
	Networks        map[string]measurementNetwork `json:"networks"`
	RollingDistinct []measurementDistinct         `json:"rolling_distinct"`
}

func measurementPointAt(at time.Time, run *snapshot.RunMetadata, byNetwork map[string][]nodeset.Row, state *distinct.State) measurementPoint {
	point := measurementPoint{
		GeneratedAt: at.Format(time.RFC3339Nano), ForkEvaluatedAt: at.Format(time.RFC3339Nano),
		SchemaVersion: snapshot.SchemaVersion, Methodology: snapshot.MethodologyVersion,
		Run: *run, Networks: make(map[string]measurementNetwork),
	}
	freshAfter := at.Add(-7 * 24 * time.Hour).Unix()
	for network, rows := range byNetwork {
		aggregate := measurementNetwork{FingerprintDirection: map[string]int{"inbound": 0, "outbound": 0}}
		for _, row := range rows {
			compatible := netconf.RowForkCurrentAt(row.Layer, network, row.ForkHash, row.ForkNext, at)
			switch row.Layer {
			case "el":
				if compatible {
					aggregate.ExecutionCurrent++
				} else {
					aggregate.ExecutionStale++
				}
			case "cl":
				if compatible {
					aggregate.ConsensusCurrent++
				} else {
					aggregate.ConsensusStale++
				}
			}
			if compatible {
				aggregate.Current++
				if row.MembershipSource == "status" {
					aggregate.MembershipVerified++
				} else {
					aggregate.MembershipClaimed++
				}
				if (row.FPStatus == "ok" || row.FPStatus == "stale") && row.FPAt >= freshAfter {
					aggregate.FingerprintIdentified++
					if row.FPDirection == "inbound" || row.FPDirection == "outbound" {
						aggregate.FingerprintDirection[row.FPDirection]++
					}
				}
			}
		}
		point.Networks[network] = aggregate
	}
	for _, estimate := range state.Estimates(at) {
		if strings.HasPrefix(estimate.Key, "walker/") {
			parts := strings.Split(estimate.Key, "/")
			if len(parts) != 4 {
				continue
			}
			point.RollingDistinct = append(point.RollingDistinct, measurementDistinct{
				Walker: parts[1], Protocol: parts[2], Family: parts[3], Estimate: estimate.Distinct, Sightings: estimate.Sightings,
				WindowStart: estimate.WindowStart.Format(time.RFC3339), WindowEnd: estimate.WindowEnd.Format(time.RFC3339), Error: estimate.Error,
			})
			continue
		}
		protocol, family, ok := strings.Cut(estimate.Key, "/")
		if !ok {
			continue
		}
		point.RollingDistinct = append(point.RollingDistinct, measurementDistinct{
			Protocol: protocol, Family: family, Estimate: estimate.Distinct, Sightings: estimate.Sightings,
			WindowStart: estimate.WindowStart.Format(time.RFC3339), WindowEnd: estimate.WindowEnd.Format(time.RFC3339), Error: estimate.Error,
		})
	}
	return point
}

func currentForkCount(network string, rows []nodeset.Row, at time.Time) int {
	current := 0
	for _, row := range rows {
		if netconf.RowForkCurrentAt(row.Layer, network, row.ForkHash, row.ForkNext, at) {
			current++
		}
	}
	return current
}
