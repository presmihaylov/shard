package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// Links hears of every change to a sandbox's links, so what serves them (host rules, the guest's hosts lines) can follow the record.
type Links interface {
	Changed(ctx context.Context, id string) error
}

// LinkNotFoundError is a target the sandbox holds no link to.
type LinkNotFoundError struct {
	Sandbox string
	Target  string
}

func (e *LinkNotFoundError) Error() string {
	return fmt.Sprintf("sandbox %s holds no link to %s: shard link list %s lists its links", e.Sandbox, e.Target, e.Sandbox)
}

func (e *LinkNotFoundError) Public() string { return e.Error() }

// AddLink links the sandbox to target at the address every link to target shares; a link it already holds answers as it is.
func (s *Service) AddLink(ctx context.Context, ref, target string) (models.Link, error) {
	id, _, unlock, err := s.holdUnfailed(ctx, ref)
	if err != nil {
		return models.Link{}, err
	}
	defer unlock()

	link, added, err := s.writeLink(id, target)
	if err != nil || !added {
		return link, err
	}
	if err := s.linksChanged(ctx, id); err != nil {
		return models.Link{}, s.refusedLink(id, link, err)
	}

	return link, nil
}

// writeLink claims the address and writes the link in one step under linksMu, so two sources linking one target share it and two targets never do.
func (s *Service) writeLink(id, target string) (models.Link, bool, error) {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	// Read again under linksMu: the remove of a target drops links from records whose lock it does not hold.
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Link{}, false, err
	}
	links, err := s.newLinks(id, []string{target})
	if err != nil {
		return models.Link{}, false, err
	}
	if i := slices.IndexFunc(sb.Links, func(l models.Link) bool { return l.TargetID == links[0].TargetID }); i >= 0 {
		return sb.Links[i], false, nil
	}
	if err := s.writeLinks(id, append(slices.Clone(sb.Links), links[0])); err != nil {
		return models.Link{}, false, err
	}

	return links[0], true, nil
}

// refusedLink takes the new link off the record again, since a record must not name a link nothing serves.
func (s *Service) refusedLink(id string, link models.Link, err error) error {
	if revert := s.unlink(id, link.TargetID); revert != nil {
		return fmt.Errorf("the link to %s was not served and stays on the record: %w", link.Target, errors.Join(err, revert))
	}

	return err
}

// RemoveLink drops the sandbox's link to target, named as the link names it or by the target's id; the address goes free once no link holds it.
func (s *Service) RemoveLink(ctx context.Context, ref, target string) error {
	id, sb, unlock, err := s.holdUnfailed(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()

	link, found, err := s.linkTo(id, target)
	if err != nil {
		return err
	}
	if !found {
		return &LinkNotFoundError{Sandbox: nameOf(id, sb), Target: target}
	}
	if err := s.unlink(id, link.TargetID); err != nil {
		return err
	}
	if err := s.linksChanged(ctx, id); err != nil {
		return fmt.Errorf("the link to %s is off the record, but %w", link.Target, err)
	}

	return nil
}

// linkTo finds the link a target names; an id the target's name resolves to finds it too.
func (s *Service) linkTo(id, target string) (models.Link, bool, error) {
	targetID, err := s.cfg.Repo.Resolve(target)
	if err != nil {
		return models.Link{}, false, err
	}

	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Link{}, false, err
	}
	i := slices.IndexFunc(sb.Links, func(l models.Link) bool { return l.Target == target || l.TargetID == targetID })
	if i < 0 {
		return models.Link{}, false, nil
	}

	return sb.Links[i], true, nil
}

func (s *Service) unlink(id, targetID string) error {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	return s.dropLink(id, targetID)
}

// dropLink takes the link to targetID off the record as it is now, whatever else changed its links since it was read.
func (s *Service) dropLink(id, targetID string) error {
	return s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Links = slices.DeleteFunc(slices.Clone(sb.Links), func(l models.Link) bool { return l.TargetID == targetID })

		return nil
	})
}

// ListLinks answers the links of one sandbox, in the order they were added.
func (s *Service) ListLinks(_ context.Context, ref string) ([]models.Link, error) {
	sb, err := Get(s.cfg.Repo, ref)
	if err != nil {
		return nil, err
	}
	if err := FailedGuard(sb.ID, sb); err != nil {
		return nil, err
	}

	return sb.Links, nil
}

// newLinks resolves each target a source names and gives it its address; the caller holds linksMu until the links are on the record.
func (s *Service) newLinks(source string, targets []string) ([]models.Link, error) {
	book, err := s.linkBook()
	if err != nil {
		return nil, err
	}

	links := make([]models.Link, 0, len(targets))
	for _, ref := range targets {
		target, err := s.linkTarget(source, ref)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(links, func(l models.Link) bool { return l.TargetID == target.ID }) {
			return nil, &RequestError{Err: fmt.Errorf("sandbox %s is linked twice; a sandbox links another once", nameOf(target.ID, target))}
		}
		address, err := book.claim(target.ID)
		if err != nil {
			return nil, err
		}
		links = append(links, models.Link{Target: nameOf(target.ID, target), TargetID: target.ID, Address: address})
	}

	return links, nil
}

// linkTarget reads the sandbox a link reaches, which must be another one that has not failed.
func (s *Service) linkTarget(source, ref string) (models.Sandbox, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}
	if id == source {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("sandbox %s cannot link itself; link another sandbox", ref)}
	}
	sb, err := s.cfg.Repo.Get(id)
	// The target is in the body or the path beside the source, so a 404 would read as the source missing.
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("sandbox %s does not exist, so nothing can link it: shard ls lists the sandboxes", ref)}
	}
	if err != nil {
		return models.Sandbox{}, err
	}
	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	return sb, nil
}

// addressBook is every link address on the host, read under linksMu, so the next claim is unique to its target.
type addressBook struct {
	byTarget map[string]netip.Addr
	taken    map[netip.Addr]bool
}

func (s *Service) linkBook() (addressBook, error) {
	sandboxes, err := sandboxstate.ListReadable(s.cfg.Repo, nil)
	if err != nil {
		return addressBook{}, err
	}

	book := addressBook{byTarget: map[string]netip.Addr{}, taken: map[netip.Addr]bool{}}
	for _, sb := range sandboxes {
		for _, l := range sb.Links {
			book.byTarget[l.TargetID] = l.Address
			book.taken[l.Address] = true
		}
	}

	return book, nil
}

// claim answers the address a link to target already holds, else the lowest one no link holds.
func (b addressBook) claim(target string) (netip.Addr, error) {
	if address, ok := b.byTarget[target]; ok {
		return address, nil
	}

	// The first and the last address of the range are its network and broadcast, which no guest can reach.
	for address := models.LinkSubnet.Addr().Next(); models.LinkSubnet.Contains(address.Next()); address = address.Next() {
		if b.taken[address] {
			continue
		}
		b.byTarget[target], b.taken[address] = address, true

		return address, nil
	}

	return netip.Addr{}, fmt.Errorf("every address in %s is held by a link: remove the links to a sandbox to free its address", models.LinkSubnet)
}

// dropLinksTo runs once the record of id is gone and under linksMu, so a link made beside the remove is either dropped here or refused; it answers the sources that lost one.
func (s *Service) dropLinksTo(id string) ([]string, error) {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	sandboxes, err := sandboxstate.ListReadable(s.cfg.Repo, nil)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, sb := range sandboxes {
		if !slices.ContainsFunc(sb.Links, func(l models.Link) bool { return l.TargetID == id }) {
			continue
		}
		if err := s.dropLink(sb.ID, id); err != nil {
			return sources, fmt.Errorf("drop the link to %s from sandbox %s: %w", id, nameOf(sb.ID, sb), err)
		}
		sources = append(sources, sb.ID)
	}

	return sources, nil
}

// createCopy writes the record of a copy; one with links takes them from the source again under linksMu, so none reaches a sandbox a remove took since.
func (s *Service) createCopy(from models.Sandbox) (models.Sandbox, error) {
	if len(from.Links) == 0 {
		return s.cfg.Repo.Create(from)
	}

	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	source, err := s.cfg.Repo.Get(from.ForkedFrom)
	if err != nil {
		return models.Sandbox{}, err
	}
	from.Links = slices.Clone(source.Links)

	return s.cfg.Repo.Create(from)
}

// linksChanged is the one place a change to the links of the sandboxes ids reaches what serves them.
func (s *Service) linksChanged(ctx context.Context, ids ...string) error {
	var errs []error
	for _, id := range ids {
		if err := s.cfg.Links.Changed(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("the links of sandbox %s did not reach what serves them: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

func (s *Service) writeLinks(id string, links []models.Link) error {
	return s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Links = links

		return nil
	})
}

// validLinks refuses a create that names no sandbox or one sandbox twice.
func validLinks(targets []string) error {
	for i, target := range targets {
		if target == "" {
			return &RequestError{Err: errors.New("a link names no target sandbox")}
		}
		if slices.Contains(targets[:i], target) {
			return &RequestError{Err: fmt.Errorf("sandbox %s is linked twice; a sandbox links another once", target)}
		}
	}

	return nil
}

// noLinks is what a service hears links with until a layer serves them: nothing.
type noLinks struct{}

func (noLinks) Changed(context.Context, string) error { return nil }
