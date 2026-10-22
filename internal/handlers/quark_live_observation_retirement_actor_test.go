//go:build quark_live

package handlers_test

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func retirementTVDirectory(ctx context.Context, d *quark.Client, parent string) ([]cloud.Item, error) {
	items := []cloud.Item{}
	seen := map[string]bool{}
	var total *int64
	for page := 0; page < 2; page++ {
		p, e := d.List(ctx, parent, cloud.PageRequest{Limit: 100, Offset: int64(page * 100)})
		if e != nil {
			return nil, e
		}
		if p.Offset != int64(page*100) || p.Total == nil || *p.Total < 0 || *p.Total > 200 || total != nil && *total != *p.Total {
			return nil, observationError()
		}
		total = p.Total
		for _, f := range p.Items {
			if f.ParentID != parent || seen[f.ID] {
				return nil, observationError()
			}
			seen[f.ID] = true
			items = append(items, f)
		}
		if !p.HasMore {
			if int64(len(items)) != *total {
				return nil, observationError()
			}
			return items, nil
		}
	}
	return nil, observationError()
}
func retirementExactEntry(items []cloud.Item, want cloud.Item, agent bool, account string) error {
	n := 0
	for _, f := range items {
		same := f.ID == want.ID
		if agent {
			actual, valid := followRawID(f.ID, quark.RouteAgent, account)
			desired, dvalid := followRawID(want.ID, quark.RouteAgent, account)
			same = valid && dvalid && quark.ObservationSameAgentHandle(actual, desired)
		}
		if same || f.Name == want.Name {
			parentSame := f.ParentID == want.ParentID
			if agent {
				a, av := followRawID(f.ParentID, quark.RouteAgent, account)
				b, bv := followRawID(want.ParentID, quark.RouteAgent, account)
				parentSame = av && bv && quark.ObservationSameAgentHandle(a, b)
			}
			if !same || !parentSame || f.Name != want.Name || f.IsDir != want.IsDir || !want.IsDir && f.Size != want.Size {
				return observationError()
			}
			n++
		}
	}
	if n != 1 {
		return observationError()
	}
	return nil
}
func retirementAbsent(items []cloud.Item, id, name, account string, agent bool) error {
	for _, f := range items {
		same := f.ID == id
		if agent {
			a, av := followRawID(f.ID, quark.RouteAgent, account)
			b, bv := followRawID(id, quark.RouteAgent, account)
			same = av && bv && quark.ObservationSameAgentHandle(a, b)
		}
		if same || f.Name == name {
			return observationError()
		}
	}
	return nil
}

func retirementVerifyTree(ctx context.Context, d *quark.Client, i retirementIdentity) error {
	c := i.Creation
	for _, v := range []struct {
		parent string
		want   cloud.Item
	}{{c.Proof.TVRoot, c.Root}, {c.Root.ID, i.Inside}} {
		items, e := retirementTVDirectory(ctx, d, v.parent)
		if e != nil {
			return e
		}
		if e = retirementExactEntry(items, v.want, false, ""); e != nil {
			return e
		}
	}
	qualify := func(raw string) string { return quark.QualifyID(quark.RouteAgent, c.AgentAccount, raw) }
	for _, v := range []struct{ parent, id, name string }{{c.ProofAgent, c.RootAgent, c.Root.Name}, {c.RootAgent, i.InsideAgent, i.DirectoryName}} {
		items, e := d.ObservationListAgentDirectory(ctx, qualify(v.parent))
		if e != nil {
			return e
		}
		if e = retirementExactEntry(items, cloud.Item{ID: qualify(v.id), ParentID: qualify(v.parent), Name: v.name, IsDir: true}, true, c.AgentAccount); e != nil {
			return e
		}
	}
	return nil
}
func retirementPrepareMove(ctx context.Context, d *quark.Client, s *retirementStore, id string) error {
	p, ok := retirementExpectedPreflight(s.state, id)
	if !ok {
		return observationError()
	}
	if e := retirementVerifyTree(ctx, d, s.state.Identity); e != nil {
		return e
	}
	c := s.state.Identity.Creation
	qualify := func(raw string) string { return quark.QualifyID(quark.RouteAgent, c.AgentAccount, raw) }
	source, e := retirementTVDirectory(ctx, d, p.SourceTV)
	if e != nil {
		return e
	}
	target, e := retirementTVDirectory(ctx, d, p.TargetTV)
	if e != nil {
		return e
	}
	want := cloud.Item{ID: p.SampleTV, ParentID: p.SourceTV, Name: p.Name, Size: p.Size}
	if retirementExactEntry(source, want, false, "") != nil || retirementAbsent(target, p.SampleTV, p.Name, "", false) != nil {
		return observationError()
	}
	source, e = d.ObservationListAgentDirectory(ctx, qualify(p.SourceAgent))
	if e != nil {
		return e
	}
	target, e = d.ObservationListAgentDirectory(ctx, qualify(p.TargetAgent))
	if e != nil {
		return e
	}
	want.ID, want.ParentID = qualify(p.SampleAgent), qualify(p.SourceAgent)
	if retirementExactEntry(source, want, true, c.AgentAccount) != nil || retirementAbsent(target, want.ID, p.Name, c.AgentAccount, true) != nil {
		return observationError()
	}
	p.At = time.Now().UTC()
	s.state.Preflights[id] = p
	return s.save()
}
func retirementConfirmPosition(ctx context.Context, d *quark.Client, s *retirementStore, id string) error {
	p, ok := retirementExpectedPreflight(s.state, id)
	if !ok {
		return observationError()
	}
	r, e := s.Load(ctx, id)
	if e != nil || r.State != "confirmed" {
		return observationError()
	}
	target, e := retirementTVDirectory(ctx, d, p.TargetTV)
	if e != nil {
		return e
	}
	source, e := retirementTVDirectory(ctx, d, p.SourceTV)
	if e != nil {
		return e
	}
	if retirementExactEntry(target, cloud.Item{ID: p.SampleTV, ParentID: p.TargetTV, Name: p.Name, Size: p.Size}, false, "") != nil || retirementAbsent(source, p.SampleTV, p.Name, "", false) != nil {
		return observationError()
	}
	return nil
}
func retirementMutate(ctx context.Context, d *quark.Client, s *retirementStore, stage string) error {
	if s.state.Completed {
		return observationError()
	}
	id := s.state.MoveOutID
	if stage == "move_back" {
		id = s.state.MoveBackID
		prior, e := s.Load(ctx, s.state.MoveOutID)
		if e != nil || prior.State != "confirmed" {
			return observationError()
		}
	} else if stage != "move_out" {
		return observationError()
	}
	_, e := s.Load(ctx, id)
	if e == nil {
		result, e := d.QueryOperation(ctx, id)
		if e != nil {
			return e
		}
		if !result.Complete || result.Receipt.State != "confirmed" {
			return observationError()
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	} else {
		if s.state.Writes[id+":move"] {
			return observationError()
		}
		if e = retirementPrepareMove(ctx, d, s, id); e != nil {
			return e
		}
		p, _ := retirementExpectedPreflight(s.state, id)
		c := s.state.Identity.Creation
		if e = d.Move(quark.WithOperationID(ctx, id), quark.QualifyID(quark.RouteAgent, c.AgentAccount, p.SampleAgent), quark.QualifyID(quark.RouteAgent, c.AgentAccount, p.TargetAgent)); e != nil {
			return e
		}
	}
	return retirementConfirmPosition(ctx, d, s, id)
}
