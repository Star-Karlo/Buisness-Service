package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// presentKeys reports which fields the caller actually supplied.
//
// This exists because "supplied" and "non-zero" are different questions, and
// only the first is the one the per-company field check asks. A quantity of 0
// and an omitted quantity are different submissions; a struct that has already
// been unmarshalled cannot tell them apart, because both leave the field at its
// zero value. So the raw body is inspected before binding.
//
// Nested objects contribute dotted paths — `route.originDistrictId` — matching
// the keys the field catalogue declares. Arrays are recorded as present by
// their own key and not descended into: the catalogue declares `items` as one
// list-typed field, not a field per element.
func presentKeys(raw []byte) map[string]bool {
	out := map[string]bool{}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// An unparseable body will fail binding a moment later with a better
		// message than anything this could produce. Returning an empty set
		// rather than an error keeps that the one place it is reported.
		return out
	}

	var walk func(prefix string, node map[string]json.RawMessage)
	walk = func(prefix string, node map[string]json.RawMessage) {
		for key, value := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}

			// An explicit null is an ABSENCE, not a value. A client clearing a
			// field sends null, and treating that as present would let it
			// satisfy a required field with nothing in it.
			if isJSONNull(value) {
				continue
			}

			out[path] = true

			var nested map[string]json.RawMessage
			if json.Unmarshal(value, &nested) == nil {
				walk(path, nested)
			}
		}
	}
	walk("", top)

	return out
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// buffered reads the body and puts it back, so it can be inspected and then
// bound.
//
// ShouldBindJSON consumes the reader, so the two cannot both have it. Restoring
// the body afterwards is what lets every existing binding call stay exactly as
// it was — this is additive, not a change to how requests are parsed.
func buffered(r *http.Request) []byte {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}

// maxBodyBytes caps what is buffered. An order with a thousand cargo lines is
// large; a body beyond this is not a form submission.
const maxBodyBytes = 8 << 20
