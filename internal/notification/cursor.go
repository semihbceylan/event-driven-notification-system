package notification

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cursor marks a position in the list ordered by (created_at DESC, id DESC).
//
// We use keyset pagination instead of OFFSET: "give me rows older than this
// one" can jump straight to the right place in the index, while OFFSET 100000
// has to read and throw away 100000 rows first. It also does not skip or
// repeat rows when new notifications are inserted between page requests.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

var ErrInvalidCursor = errors.New("invalid cursor")

// Encode turns the cursor into an opaque string for clients. Postgres stores
// microseconds, so that is the precision we keep.
func (c Cursor) Encode() string {
	raw := strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	micros, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	us, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: time.UnixMicro(us).UTC(), ID: parsedID}, nil
}
