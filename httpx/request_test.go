package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDecodeJSON(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	tests := []struct {
		name       string
		body       string
		wantStatus int // 0 when DecodeJSON should succeed
		wantCode   string
	}{
		{"valid", `{"name":"Groceries"}`, 0, ""},
		{"whitespace around the value", " {\"name\":\"Groceries\"}\n", 0, ""},
		{"empty", ``, http.StatusBadRequest, "invalid_json"},
		{"not JSON", `name=Groceries`, http.StatusBadRequest, "invalid_json"},
		{"unknown field", `{"name":"Groceries","color":"red"}`, http.StatusBadRequest, "invalid_json"},
		{"wrong type", `{"name":1}`, http.StatusBadRequest, "invalid_json"},
		{"two values", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest, "invalid_json"},
		{"garbage after the value", `{"name":"a"} x`, http.StatusBadRequest, "invalid_json"},
		{"over 1 MB", `{"name":"` + strings.Repeat("a", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/lists", strings.NewReader(tt.body))
			var got body
			ok := DecodeJSON(rec, req, &got)

			if tt.wantStatus == 0 {
				if !ok || got.Name != "Groceries" || rec.Body.Len() != 0 {
					t.Errorf("DecodeJSON = %v, %+v, response %q; want true, the name and no response", ok, got, rec.Body)
				}
				return
			}
			if ok {
				t.Fatalf("DecodeJSON = true, want false")
			}
			var resp struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response %q isn't the error shape: %v", rec.Body, err)
			}
			if rec.Code != tt.wantStatus || resp.Error.Code != tt.wantCode || resp.Error.Message == "" {
				t.Errorf("response = %d %s, want %d with code %s", rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestNullableID(t *testing.T) {
	id := uuid.MustParse("019a7000-0000-7000-8000-000000000001")
	tests := []struct {
		body        string
		wantPresent bool
		wantID      *uuid.UUID
		wantErr     bool
	}{
		{`{}`, false, nil, false},
		{`{"afterId":null}`, true, nil, false},
		{`{"afterId":"019a7000-0000-7000-8000-000000000001"}`, true, &id, false},
		{`{"afterId":"nope"}`, false, nil, true},
		{`{"afterId":""}`, false, nil, true},
		{`{"afterId":7}`, false, nil, true},
	}
	for _, tt := range tests {
		var got struct {
			AfterID NullableID `json:"afterId"`
		}
		err := json.Unmarshal([]byte(tt.body), &got)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%s: no error, want one", tt.body)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.body, err)
			continue
		}
		if got.AfterID.Present != tt.wantPresent || (got.AfterID.ID == nil) != (tt.wantID == nil) ||
			(tt.wantID != nil && *got.AfterID.ID != *tt.wantID) {
			t.Errorf("%s: got %+v, want present %v and id %v", tt.body, got.AfterID, tt.wantPresent, tt.wantID)
		}
	}
}

func TestOptional(t *testing.T) {
	type body struct {
		Title Optional[string] `json:"title"`
		Done  Optional[bool]   `json:"done"`
	}
	tests := []struct {
		body    string
		want    body
		wantErr bool
	}{
		{`{}`, body{}, false},
		{`{"title":"Milk"}`, body{Title: Optional[string]{Value: "Milk", Set: true}}, false},
		// An empty string or false is a value, unlike a missing field.
		{`{"title":"","done":false}`, body{Title: Optional[string]{Set: true}, Done: Optional[bool]{Set: true}}, false},
		{`{"done":true}`, body{Done: Optional[bool]{Value: true, Set: true}}, false},
		{`{"title":null}`, body{}, true},
		{`{"done":null}`, body{}, true},
		{`{"title":7}`, body{}, true},
		{`{"done":"yes"}`, body{}, true},
	}
	for _, tt := range tests {
		var got body
		err := json.Unmarshal([]byte(tt.body), &got)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%s: no error, want one", tt.body)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: got %+v, %v; want %+v", tt.body, got, err, tt.want)
		}
	}
}
