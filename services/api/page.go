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

// pageOf reads the page query, and refuses a cursor that could never be a key of the list, by the list's shape.
func pageOf(r *http.Request, shape func(string) error) (pageQuery, error) {
	query := r.URL.Query()
	q := pageQuery{cursor: query.Get("cursor")}

	if q.cursor != "" {
		if err := shape(q.cursor); err != nil {
			return pageQuery{}, &sandbox.RequestError{Err: fmt.Errorf("the query cursor is malformed: %w", err)}
		}
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
		return items, nil
	}
	last := key(items[q.limit-1])

	return items[:q.limit], &last
}
