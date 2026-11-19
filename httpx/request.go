package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// MaxBodyBytes caps every request body (README.md §7).
const MaxBodyBytes = 1 << 20

// DecodeJSON reads r's body into v: exactly one JSON value, with no fields v doesn't have, and at
// most MaxBodyBytes. On failure it answers 400 invalid_json (413 body_too_large for a body over
// the cap) and returns false, and the handler should return.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) {
		err = errors.New("the body is empty")
	}
	if err == nil {
		// Anything after the value, even a second value, makes the body invalid.
		if extra := dec.Decode(&json.RawMessage{}); !errors.Is(extra, io.EOF) {
			err = errors.New("data after the JSON value")
			if extra != nil {
				err = extra
			}
		}
	}
	if err == nil {
		return true
	}
	if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
		WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large",
			fmt.Sprintf("Request body is over %d bytes", tooBig.Limit))
		return false
	}
	WriteError(w, http.StatusBadRequest, "invalid_json", "Request body isn't valid JSON for this route: "+err.Error())
	return false
}

// NullableID is a request field that must be present and holds an id or null, like a move's
// afterId (null for the top). Present is false when the body doesn't have the field at all.
type NullableID struct {
	// ID is nil when the field is null.
	ID      *uuid.UUID
	Present bool
}

// UnmarshalJSON implements json.Unmarshaler. The decoder calls it for null too.
func (n *NullableID) UnmarshalJSON(b []byte) error {
	n.Present, n.ID = true, nil
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("an id must be a string or null")
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return fmt.Errorf("%q isn't an id", s)
	}
	n.ID = &id
	return nil
}

// Optional is a request field that may be left out but is never null, like the fields of a PATCH.
// Leaving a field out keeps its value, so a null would be ambiguous: did the client mean to clear
// it? Set is false when the body doesn't have the field.
type Optional[T any] struct {
	Value T
	Set   bool
}

// UnmarshalJSON implements json.Unmarshaler. The decoder calls it for null too, which it rejects.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return errors.New("a field can't be null; leave it out instead")
	}
	if err := json.Unmarshal(b, &o.Value); err != nil {
		return err
	}
	o.Set = true
	return nil
}
