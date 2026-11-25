// seatimeload is a load generator and capacity model for seatimed.
//
//	seatimeload model                      print the derived peak-load model
//	seatimeload seed  -db F -users F       build a realistic pre-populated database + users file
//	seatimeload run   -url U -users F      drive writers (sync/segments/register) and readers (pages/API/SSE)
//	seatimeload dbstat -db F               per-table/index bytes and rows
//
// It speaks only the public wire protocol (pkg/api) except for `seed` and
// `dbstat`, which touch the SQLite file directly while seatimed is stopped.
package main

import (
	"fmt"
	"math"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "model":
		cmdModel(os.Args[2:])
	case "seed":
		cmdSeed(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "dbstat":
		cmdDBStat(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: seatimeload model|seed|run|dbstat [flags]")
	os.Exit(2)
}

// ---- demand model ----

// Hackatime's real numbers, the hosting requirement's yardstick.
const (
	MAU          = 15488
	MonthlyHours = 109759.0
)

// diurnal returns the share of a day's activity falling in each UTC hour. Hack
// Club skews to US teenagers (evening Eastern, peak ~01 UTC) with a European /
// Indian shoulder in the UTC afternoon and a small overnight floor.
func diurnal() [24]float64 {
	g := func(h, mu, sigma float64) float64 {
		d := math.Abs(h - mu)
		d = math.Min(d, 24-d)
		return math.Exp(-d * d / (2 * sigma * sigma))
	}
	var w [24]float64
	var sum float64
	for h := 0; h < 24; h++ {
		x := float64(h)
		w[h] = 0.08 + 0.92*(0.55*g(x, 1.5, 3.2)+0.30*g(x, 16, 3.5)+0.15*g(x, 10, 3))
		sum += w[h]
	}
	for h := range w {
		w[h] /= sum
	}
	return w
}

// PeakDayFactor: the busiest day of the month versus the average day
// (weekends, deadline surges before a YSWS ends).
const PeakDayFactor = 1.5

type Model struct {
	AvgHoursPerDay float64
	PeakHourShare  float64
	PeakDayFactor  float64
	PeakConcurrent float64 // logged-hours per wall-hour at the busiest hour of the busiest day
	HoursPerUser   float64
	SyncsPerSec1x  float64
	BucketsPerSync float64
}

func buildModel() Model {
	w := diurnal()
	pk := 0.0
	for _, v := range w {
		pk = math.Max(pk, v)
	}
	avg := MonthlyHours / 30
	m := Model{AvgHoursPerDay: avg, PeakHourShare: pk, PeakDayFactor: PeakDayFactor, HoursPerUser: MonthlyHours / MAU}
	m.PeakConcurrent = avg * pk * PeakDayFactor
	m.SyncsPerSec1x = m.PeakConcurrent / 90
	m.BucketsPerSync = 30 * 0.85 * 1.2
	return m
}

func cmdModel(args []string) {
	m := buildModel()
	w := diurnal()
	fmt.Printf("MAU %d, %.0f logged hours/month (%.2f h/user/month)\n", MAU, MonthlyHours, m.HoursPerUser)
	fmt.Printf("average day %.0f h; busiest hour carries %.1f%% of a day (peak/mean %.2fx); peak-day factor %.1fx\n",
		m.AvgHoursPerDay, 100*m.PeakHourShare, m.PeakHourShare*24, m.PeakDayFactor)
	fmt.Printf("1x = busiest hour of the busiest day = %.0f concurrently-active clients = %.1f syncs/s (each ~%.0f buckets)\n",
		m.PeakConcurrent, m.SyncsPerSec1x, m.BucketsPerSync)
	fmt.Print("UTC hour share %: ")
	for h, v := range w {
		fmt.Printf("%02d:%.1f ", h, 100*v)
	}
	fmt.Println()
}
