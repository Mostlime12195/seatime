package daemon

import "runtime"

type debugStats struct{ heap uint64 }

func (d *debugStats) read() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	d.heap = m.HeapAlloc
}
