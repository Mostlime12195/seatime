package server

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/promptcheck"
)

// clockSkew bounds how far a client's commit time may run ahead of the server.
const clockSkew = audit.ClockSkew

// Analysis is the server's view of a segment: the shared analysis
// (pkg/audit, the same code the device counts its claim with) plus what
// only the server knows: the device's checkpoints, other devices that sent
// the same chain, and the minutes earlier segments of the account already
// credit.
type Analysis struct {
	*audit.Analysis
	Checkpoints []api.ServerCheckpoint
	Prev        *api.ServerCheckpoint
	// Claimed are the minutes earlier segments of this account credited
	// when this one was analysed (fed to audit's Overlap).
	Claimed    map[int64]float64
	NetMinutes map[int64]float64 // credited wall seconds per minute, after overlap
	// PromptCheck is the prompt check's switch and verdicts the count
	// used (nil while it is off).
	PromptCheck *promptcheck.Set
}

// analyze runs the shared analysis on a segment against the checkpoints
// this device synced in real time, then takes out minutes earlier segments
// already credit. Only segments created before ref are compared.
func (s *Server) analyze(ctx context.Context, dev *store.Device, b *api.Bundle, ref *store.Segment) (*Analysis, error) {
	in := audit.Input{Bundle: *b, DeviceCreated: dev.Created}
	pc, err := s.promptChecks(ctx, dev.ID, b)
	if err != nil {
		return nil, err
	}
	in.PromptCheck = pc
	if len(b.Entries) > 0 {
		lo, hi := b.Entries[0].Seq, b.Entries[len(b.Entries)-1].Seq
		all, err := s.store.Checkpoints(ctx, dev.ID, 0, hi)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			sc := api.ServerCheckpoint{Seq: c.Seq, Hash: c.Hash, ServerTS: c.ServerTS}
			if c.Seq >= lo {
				in.Checkpoints = append(in.Checkpoints, sc)
			} else {
				in.Prev = &sc
			}
		}
	}
	a := &Analysis{Analysis: audit.Analyze(in), Checkpoints: in.Checkpoints, Prev: in.Prev, PromptCheck: in.PromptCheck}
	before, exclude := ref.Created, ref.ID
	if err := s.sharedChain(ctx, dev, a); err != nil {
		return nil, err
	}
	if err := s.claimedMinutes(ctx, dev, a, before, exclude); err != nil {
		return nil, err
	}
	return a, nil
}

// sharedChain fails verification when a device of another account sent
// checkpoints of this same chain: a copied device key or a mirrored chain.
func (s *Server) sharedChain(ctx context.Context, dev *store.Device, a *Analysis) error {
	var hashes []string
	for _, c := range a.Checkpoints {
		hashes = append(hashes, c.Hash)
	}
	owners, err := s.store.CheckpointOwners(ctx, hashes, dev.ID)
	if err != nil {
		return err
	}
	other := 0
	for _, o := range owners {
		if o.UserID != dev.UserID {
			other++
		}
	}
	if other > 0 {
		a.Problems = append(a.Problems, fmt.Sprintf("this chain's checkpoints were also sent by %d device(s) of another account: a copied device key or mirrored chain", other))
	}
	return nil
}

// claimedMinutes takes out minutes already credited to this account by
// earlier segments (two devices, several projects, or the same records
// sent again): no minute counts twice.
func (s *Server) claimedMinutes(ctx context.Context, dev *store.Device, a *Analysis, before time.Time, exclude string) error {
	mine := a.Minutes()
	a.Claimed = map[int64]float64{}
	if len(mine) > 0 {
		lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
		for m := range mine {
			lo, hi = min(lo, m), max(hi, m)
		}
		claims, err := s.store.MinuteClaims(ctx, dev.UserID, lo, hi, before, exclude)
		if err != nil {
			return err
		}
		subs := map[string]bool{}
		for _, c := range claims {
			if _, ok := mine[c.StartMin]; ok {
				a.Claimed[c.StartMin] += c.Human
				subs[c.SegmentID] = true
			}
		}
		a.Overlap(a.Claimed, sortedSet(subs))
	}
	a.NetMinutes = a.Minutes()
	return nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
