// In-memory store for the github-core-clone-01 Go forge.
// Port of the Node entry's store.ts, including the learned-owner rule.
//
// OWNERSHIP. The suite's bearer token is opaque and the login it names
// (the "viewer") is a generated name the entry cannot derive from the
// token or the environment, so tokenOwner defaults to
// MAKEFUN_SHOP_OWNER ?? "adaiversen" (the suite's fixed viewer login)
// and is only a fallback. Repositories are therefore created under
// that fallback owner, and the real owner is LEARNED: the first
// owner-scoped access to a repository name claims that name for that
// owner, and every other owner then gets 404 for it (never 403).
// resolveRepo is the single lookup every surface must use.

package main

import (
	"os"
)

type Store struct {
	Repos      map[string]*RepoRecord // key "owner/name"
	TokenOwner string
	nextRepoID int
	learned    map[string]string // repository name -> login
	order      []*RepoRecord     // repos in creation order (ListRepos must be stable for pagination)
}

func NewStore() *Store {
	owner := os.Getenv("MAKEFUN_SHOP_OWNER")
	if owner == "" {
		owner = "adaiversen"
	}
	return &Store{
		Repos:      make(map[string]*RepoRecord),
		TokenOwner: owner,
		nextRepoID: 1,
		learned:    make(map[string]string),
	}
}

func (s *Store) repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) CreateRepo(owner, name, description string) *RepoRecord {
	if existing := s.GetRepo(owner, name); existing != nil {
		return existing
	}
	rec := newRepoRecord(owner, name, description)
	rec.ID = s.nextRepoID
	s.nextRepoID++
	rec.Private = false
	s.Repos[s.repoKey(owner, name)] = rec
	s.order = append(s.order, rec)
	return rec
}

func (s *Store) GetRepo(owner, name string) *RepoRecord {
	return s.Repos[s.repoKey(owner, name)]
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := s.repoKey(owner, name)
	rec, ok := s.Repos[key]
	if !ok {
		return false
	}
	delete(s.Repos, key)
	for i, r := range s.order {
		if r == rec {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

func (s *Store) ListRepos(owner string) []*RepoRecord {
	var out []*RepoRecord
	for _, repo := range s.order {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	return out
}

// ResolveRepo applies the learned-owner rule. Returns nil when the name
// does not exist, or when it belongs to a different (already learned) owner.
func (s *Store) ResolveRepo(owner, name string) *RepoRecord {
	if direct := s.GetRepo(owner, name); direct != nil {
		// A direct hit claims the name for this owner too.
		if _, ok := s.learned[name]; !ok {
			s.learned[name] = owner
		}
		return direct
	}
	if claimed, ok := s.learned[name]; ok {
		if claimed != owner {
			return nil
		}
		for _, repo := range s.Repos {
			if repo.Name == name {
				return repo
			}
		}
		return nil
	}
	for _, repo := range s.Repos {
		if repo.Name == name {
			s.learned[name] = owner
			return repo
		}
	}
	return nil
}

// RepoByName finds a repository by name alone, regardless of owner.
func (s *Store) RepoByName(name string) *RepoRecord {
	for _, repo := range s.Repos {
		if repo.Name == name {
			return repo
		}
	}
	return nil
}

// DisplayOwner is the owner login to display for a repository:
// learned owner, else stored owner.
func (s *Store) DisplayOwner(repo *RepoRecord) string {
	if owner, ok := s.learned[repo.Name]; ok {
		return owner
	}
	return repo.Owner
}
