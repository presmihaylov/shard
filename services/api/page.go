package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/presmihaylov/shard/services/sandbox"
)

// pageQuery is ?limit and ?cursor on a list: no limit is the whole list, the cursor the last id of the page before.
type pageQuery struct {
	limit  int
	cursor string
}

func pageOf(r *http.Request) (pageQuery, error) {
	query := r.URL.Query()
	q := pageQuery{cursor: query.Get("cursor")}

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

// page cuts the items after the cursor, caps them at the limit, and names the last one only when more remain.
func page[T any](items []T, q pageQuery, id func(T) string) ([]T, *string, error) {
	if q.cursor != "" {
		at := slices.IndexFunc(items, func(item T) bool { return id(item) == q.cursor })
		if at < 0 {
			return nil, nil, &sandbox.RequestError{Err: fmt.Errorf("the cursor %q names nothing in this list", q.cursor)}
		}
		items = items[at+1:]
	}
	if q.limit == 0 || len(items) <= q.limit {
		return items, nil, nil
	}
	last := id(items[q.limit-1])

	return items[:q.limit], &last, nil
}
