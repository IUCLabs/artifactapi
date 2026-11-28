package keycrypt

import "time"

func (t *Transit) SetClock(now func() time.Time) { t.now = now }
