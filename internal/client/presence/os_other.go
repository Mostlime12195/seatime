//go:build !linux && !darwin && !windows

package presence

func terminal() Terminal                  { return nil }
func windows(func(string) string) Windows { return nil }
