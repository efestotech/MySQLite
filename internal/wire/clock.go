package wire

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/tz"
	"mysqlite/internal/vars"
)

// statementClock is the time of the running statement (ADR 0007): read once from
// @@timestamp, so every current-time function of a statement sees the same instant
// and SET timestamp freezes it, as in MySQL.
type statementClock struct {
	set bool
	t   time.Time
}

func (h *handler) now() time.Time {
	if h.clock.set {
		return h.clock.t
	}
	t := time.Now()
	if v, _, e := h.vars.Get("timestamp", vars.ReadSession); e == nil {
		if f, err := strconv.ParseFloat(v.Text, 64); err == nil {
			t = time.UnixMicro(int64(math.Round(f * 1e6)))
		}
	}
	h.clock = statementClock{set: true, t: t}
	return t
}

// sessionZone is the location of the session's time_zone; the host's zone if it cannot
// be read (SET validates every value tz accepts).
func (h *handler) sessionZone() *time.Location {
	v, _, e := h.vars.Get("time_zone", vars.ReadSession)
	if e != nil {
		return time.Local
	}
	loc, err := tz.Location(v.Text)
	if err != nil {
		h.log.Warn("time_zone not resolved", "value", v.Text, "error", err.Error())
		return time.Local
	}
	return loc
}

// clockValue is a current-time function's value, as MySQL prints it, with fsp
// fractional digits (truncated).
func (h *handler) clockValue(name string, fsp int) any {
	t := h.now()
	if name == functions.SessionUnixTimestamp {
		return t.Unix()
	}
	switch name {
	case functions.SessionUTCNow, functions.SessionUTCDate, functions.SessionUTCTime:
		t = t.UTC()
	default:
		t = t.In(h.sessionZone())
	}
	frac := ""
	if fsp > 0 {
		frac = "." + fmt.Sprintf("%09d", t.Nanosecond())[:fsp]
	}
	switch name {
	case functions.SessionCurDate, functions.SessionUTCDate:
		return t.Format("2006-01-02")
	case functions.SessionCurTime, functions.SessionUTCTime:
		return t.Format("15:04:05") + frac
	}
	return t.Format("2006-01-02 15:04:05") + frac
}
