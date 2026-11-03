package browse

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// Branch is one branch.
type Branch struct {
	Name    string
	Commit  Commit
	Default bool
}

// Tag is one tag.
type Tag struct {
	Name      string
	Commit    *Commit // nil if the tag does not point to a commit
	Annotated bool
	Message   string // message of an annotated tag
}

// BranchPage is one page of ListBranches.
type BranchPage struct {
	Branches      []Branch
	NextPageToken string
}

// TagPage is one page of ListTags.
type TagPage struct {
	Tags          []Tag
	NextPageToken string
}

// ListBranches returns one page of branches whose name contains query
// (case-insensitively): the default branch first, then by name.
func (s *Service) ListBranches(ctx context.Context, id uuid.UUID, query string, pageSize int32, pageToken string) (BranchPage, error) {
	if len(query) > maxQueryLen {
		return BranchPage{}, invalid("query", fmt.Sprintf("must be at most %d bytes", maxQueryLen))
	}
	size, after, err := pageArgs(id, pageSize, pageToken)
	if err != nil {
		return BranchPage{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return BranchPage{}, err
	}
	records, err := s.forEachRef(ctx, r.dir, "refs/heads/", append([]string{"refname:strip=2"}, commitAtoms...))
	if err != nil {
		return BranchPage{}, err
	}

	query = strings.ToLower(query)
	var items []sorted[Branch]
	for _, rec := range records {
		name := clean(rec[0])
		if !strings.Contains(strings.ToLower(name), query) {
			continue
		}
		c, err := parseCommit(rec[1:])
		if err != nil {
			return BranchPage{}, err
		}
		b := Branch{Name: name, Commit: c, Default: rec[0] == r.defaultBranch}
		key := refKey{Rank: 1, Name: rec[0]}
		if b.Default {
			key.Rank = 0
		}
		items = append(items, sorted[Branch]{key: key, item: b})
	}
	page, next := paginate(items, after, size, id)
	return BranchPage{Branches: page, NextPageToken: next}, nil
}

// tagAtoms describe a tag: its name and type, its date and message, then
// the commit it points to through an annotated tag (the "*" atoms), then
// the commit a lightweight tag points to directly.
var tagAtoms = func() []string {
	atoms := []string{"refname:strip=2", "objecttype", "creatordate:unix", "contents", "*objecttype"}
	for _, a := range commitAtoms {
		atoms = append(atoms, "*"+a)
	}
	return append(atoms, commitAtoms...)
}()

// ListTags returns one page of tags, newest first, then by name.
func (s *Service) ListTags(ctx context.Context, id uuid.UUID, pageSize int32, pageToken string) (TagPage, error) {
	size, after, err := pageArgs(id, pageSize, pageToken)
	if err != nil {
		return TagPage{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return TagPage{}, err
	}
	records, err := s.forEachRef(ctx, r.dir, "refs/tags/", tagAtoms)
	if err != nil {
		return TagPage{}, err
	}

	items := make([]sorted[Tag], 0, len(records))
	for _, rec := range records {
		t := Tag{Name: clean(rec[0])}
		const peeled, direct = 5, 5 + commitFields
		switch rec[1] {
		case "tag":
			t.Annotated = true
			t.Message = clean(strings.TrimRight(rec[3], "\n"))
			if rec[4] == "commit" {
				c, err := parseCommit(rec[peeled : peeled+commitFields])
				if err != nil {
					return TagPage{}, err
				}
				t.Commit = &c
			}
		case "commit":
			c, err := parseCommit(rec[direct : direct+commitFields])
			if err != nil {
				return TagPage{}, err
			}
			t.Commit = &c
		}
		// A missing date (a tag object without a tagger) sorts as oldest.
		date, _ := strconv.ParseInt(rec[2], 10, 64)
		items = append(items, sorted[Tag]{key: refKey{Time: date, Name: rec[0]}, item: t})
	}
	page, next := paginate(items, after, size, id)
	return TagPage{Tags: page, NextPageToken: next}, nil
}

// refKey orders refs: by Rank, then newest Time first, then by Name in
// byte order. Name is the ref's raw name, as git printed it.
type refKey struct {
	Rank int    `json:"k,omitempty"`
	Time int64  `json:"t,omitempty"`
	Name string `json:"n"`
}

func compareKeys(a, b refKey) int {
	return cmp.Or(cmp.Compare(a.Rank, b.Rank), cmp.Compare(b.Time, a.Time), strings.Compare(a.Name, b.Name))
}

// sorted is an item with its sort key.
type sorted[T any] struct {
	key  refKey
	item T
}

// refCursor is the position after the last ref of a page. The next page
// starts after Key, so it stays correct when refs change between requests.
type refCursor struct {
	Repo uuid.UUID `json:"r"` // the token is only valid for this repository
	Key  refKey    `json:"a"`
}

// pageArgs checks the page size and decodes the page token, if any.
func pageArgs(id uuid.UUID, pageSize int32, token string) (int, *refKey, error) {
	size, err := repo.PageSize(pageSize)
	if err != nil {
		return 0, nil, err
	}
	if token == "" {
		return size, nil, nil
	}
	var c refCursor
	if err := decodeToken(token, &c); err != nil || c.Repo != id {
		return 0, nil, invalid("page_token", "is invalid or was issued for another repository")
	}
	return size, &c.Key, nil
}

// paginate sorts items and returns those after the cursor, at most size,
// with the token for the next page.
func paginate[T any](items []sorted[T], after *refKey, size int, id uuid.UUID) ([]T, string) {
	slices.SortFunc(items, func(a, b sorted[T]) int { return compareKeys(a.key, b.key) })
	if after != nil {
		start, _ := slices.BinarySearchFunc(items, *after, func(e sorted[T], k refKey) int {
			if compareKeys(e.key, k) <= 0 {
				return -1
			}
			return 1
		})
		items = items[start:]
	}
	next := ""
	if len(items) > size {
		items = items[:size]
		next = encodeToken(refCursor{Repo: id, Key: items[size-1].key})
	}
	page := make([]T, len(items))
	for i, it := range items {
		page[i] = it.item
	}
	return page, next
}
