package main

import (
	"encoding/json"
	"io"
)

// writeNDJSON is a list under --json: one document per line, no enclosing
// array, and nothing at all when the list is empty — the shape every --json
// answer has (BB-035, fourth 2026-11-03 list), so a reader parses line by line
// and never needs to know which commands answered with an array.
func writeNDJSON[T any](w io.Writer, items []T) error {
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}
