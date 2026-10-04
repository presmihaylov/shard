package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/presmihaylov/shard/services/sandbox"
)

// pageQuery is ?limit and ?cursor on a list: no limit is the whole list, the cursor a position in its order.
type pageQuery struct {
	limit  int
	cursor string
}

// pageInput is the page query of a public list, whose limit Huma checks before the handler runs.
type pageInput struct {
	Limit  int    `query:"limit" minimum:"1" doc:"The most rows a page holds; none answers the whole list."`
	Cursor string `query:"cursor" doc:"The next of the page before; this page starts after it."`
}

// paged refuses a cursor that could never be a key of the list, by the list's shape.
func paged(limit int, cursor string, shape func(string) error) (pageQuery, error) {
	if cursor == "" {
		return pageQuery{limit: limit}, nil
	}

	if err := shape(cursor); err != nil {
		return pageQuery{}, &sandbox.RequestError{Err: fmt.Errorf("the query cursor is malformed: %w", err)}
	}

	return pageQuery{limit: limit, cursor: cursor}, nil
}

// pageOf reads the page query of a local list, which Huma never sees.
func pageOf(r *http.Request, shape func(string) error) (pageQuery, error) {
	query := r.URL.Query()
	q, err := paged(0, query.Get("cursor"), shape)
	if err != nil {
		return pageQuery{}, err
	}

	raw := query.Get("limit")
	if raw == "" {
		return q, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return pageQuery{}, &sandbox.RequestError{Err: fmt.Errorf("the query limit=%q is not a count of at least 1", raw)}
	}
	q.limit = limit

	return q, nil
}

// page keeps the items whose key sorts after the cursor, caps them at the limit, and names the last one only when more remain.
func page[T any](items []T, q pageQuery, key func(T) string) ([]T, *string) {
	// The list is sorted by its key, so a cursor is a position even when the row it named is gone.
	from := sort.Search(len(items), func(i int) bool { return key(items[i]) > q.cursor })
	items = items[from:]

	if q.limit == 0 || len(items) <= q.limit {
		return listOf(items), nil
	}
	last := key(items[q.limit-1])

	return items[:q.limit], &last
}

// listOf answers an empty list as [], which the spec promises, never as null.
func listOf[T any](items []T) []T {
	if items == nil {
		return []T{}
	}

	return items
}
