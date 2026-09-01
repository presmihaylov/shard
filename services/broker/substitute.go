package broker

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/presmihaylov/shard/services/secret"
)

// maxBody bounds what is read into memory to find a placeholder. A larger body goes out as it is,
// placeholder and all, because a secret is put in headers and small bodies and never in an upload.
const maxBody = 8 << 20

type substitution struct {
	mock    string
	value   string
	headers []secret.Header
	match   *matcher
}

// rewrite puts every value in place of its placeholder, in the path, the query, the headers and a
// bounded body. Nil when there is nothing to put in, so the proxy has nothing to call.
func rewrite(subs []substitution) func(*http.Request) error {
	if len(subs) == 0 {
		return nil
	}

	replacer := newReplacer(subs)

	return func(r *http.Request) error {
		for _, sub := range subs {
			if !sub.match.matches(r) {
				continue
			}
			for _, h := range sub.headers {
				r.Header.Set(h.Name, strings.ReplaceAll(h.Value, "{value}", sub.value))
			}
		}

		r.URL.Path = replacer.Replace(r.URL.Path)
		r.URL.RawPath = replacer.Replace(r.URL.RawPath)
		r.URL.RawQuery = replacer.Replace(r.URL.RawQuery)

		for key, values := range r.Header {
			for i, value := range values {
				values[i] = replacer.Replace(value)
			}
			r.Header[key] = values
		}

		return rewriteBody(r, replacer)
	}
}

func newReplacer(subs []substitution) *strings.Replacer {
	pairs := make([]string, 0, 2*len(subs))
	for _, sub := range subs {
		pairs = append(pairs, sub.mock, sub.value)
	}

	return strings.NewReplacer(pairs...)
}

func rewriteBody(r *http.Request, replacer *strings.Replacer) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}

	head, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("read the request body: %w", err)
	}

	// Over the bound: what was read goes out first, then the rest streams, and nothing is changed.
	if len(head) > maxBody {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}

		return nil
	}

	body := []byte(replacer.Replace(string(head)))
	r.Body = io.NopCloser(bytes.NewReader(body))
	// The transport writes ContentLength and drops the header, so the field is the one that counts.
	r.ContentLength = int64(len(body))

	return nil
}
