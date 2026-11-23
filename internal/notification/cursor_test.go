package notification

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{
		CreatedAt: time.Date(2026, 11, 23, 12, 30, 45, 123456000, time.UTC),
		ID:        uuid.Must(uuid.NewV7()),
	}
	got, err := DecodeCursor(c.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) || got.ID != c.ID {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, c)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!!", "bm90LWEtY3Vyc29y" /* "not-a-cursor" */} {
		if _, err := DecodeCursor(s); err == nil {
			t.Errorf("DecodeCursor(%q) should fail", s)
		}
	}
}
