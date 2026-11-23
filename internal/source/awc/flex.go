package awc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// AWC is inconsistent about JSON types: the same logical field is a number in
// one feed and a quoted string in another, and times are epoch seconds in some
// places and ISO strings in others. These types accept both.

// flexFloat decodes a JSON number or a numeric string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("not a number: %s", b)
	}
	*f = flexFloat(v)
	return nil
}

// flexTime decodes epoch seconds (number) or an RFC 3339 string.
type flexTime struct{ time.Time }

// Epoch values above this are implausible as seconds (year ~5138) and likely
// milliseconds; reject rather than guess.
const maxEpochSeconds = 1e11

func (t *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
		// A numeric string is also accepted as epoch seconds.
		b = []byte(strings.TrimSpace(s))
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil || v <= 0 || v > maxEpochSeconds {
		return fmt.Errorf("not a time: %s", b)
	}
	t.Time = time.Unix(int64(v), 0).UTC()
	return nil
}

// flexString decodes a string, number or bool into its text form.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	*s = flexString(b)
	return nil
}
