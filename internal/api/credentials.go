package api

// A write-only credential field tells the client one thing today: whether a
// value is stored. That was enough while a stored value was always readable.
//
// After a lost or wrong ENCRYPTION_KEY it stops being enough, and quietly
// becomes a trap: every settings form treats an empty field as "keep the stored
// value", so an administrator who saves without retyping believes they
// preserved something and has preserved nothing. The column is non-empty, so
// the form says "set", and the value behind it is rubbish.
//
// The key id on the record is what makes the difference visible (ADR §62), so
// each `*_set` boolean gains a companion. `set` still means exactly what it
// meant — there is a value in the column — and `unreadable` is the new fact,
// which is why this is a sibling field rather than a change of meaning to an
// existing one.
//
// Not a health check: it asks whether this install holds the key the record
// names, which is the question the form needs. Whether the bytes are intact is
// a different question and only a decrypt answers it.
func (s *Server) credentialUnreadable(record string) bool {
	return record != "" && !s.encryptor.CanRead(record)
}
