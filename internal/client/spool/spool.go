// Package spool is the hooks' fallback queue. A hook must never block or slow
// the user's agent harness, so when the local database is locked, corrupt or
// slow, the hook writes its raw events to a small file here and exits. The
// daemon feeds spooled files through the normal pipeline later.
//
// Each spool file is written to a temporary name and renamed into place, so a
// reader only ever sees complete files. Files hold raw (pre-pipeline) events,
// one JSON object per line, with prompt text: the directory is 0700 and the
// files 0600, like the database itself.
package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"seatime/pkg/event"
)

const (
	// MaxFiles bounds the queue. Past it new events are dropped (and logged by
	// the caller): a daemon that has been down for weeks must not fill the disk.
	MaxFiles = 5000
	// MaxFileBytes bounds one spool file (a single hook writes a handful of events).
	MaxFileBytes = 4 << 20
	suffix       = ".jsonl"
)

// ErrFull means the spool already holds MaxFiles files.
var ErrFull = errors.New("spool is full")

// Write stores evs as one new spool file in dir. It never blocks on locks.
func Write(dir string, evs []event.Event) error {
	if len(evs) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if n, _ := Count(dir); n >= MaxFiles {
		return ErrFull
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			return err
		}
		if buf.Len() > MaxFileBytes {
			return fmt.Errorf("spool: batch larger than %d bytes", MaxFileBytes)
		}
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	name := fmt.Sprintf("%019d-%d%s", time.Now().UnixNano(), os.Getpid(), suffix)
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Count returns the number of complete spool files in dir (0 if none).
func Count(dir string) (int, error) {
	files, err := list(dir)
	return len(files), err
}

func list(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), suffix) && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // names start with a zero-padded timestamp: oldest first
	return out, nil
}

// Drain reads every complete spool file in dir, oldest first, and hands its
// events to fn. A file is removed once fn accepted all of its events; if fn
// fails the file stays for the next drain and Drain stops. Lines that are not
// valid events are skipped (a half-written file cannot exist, but a
// hand-edited one can). Stale temporary files older than an hour are removed.
func Drain(dir string, fn func(event.Event) error) (n int, err error) {
	files, err := list(dir)
	if err != nil {
		return 0, err
	}
	for _, name := range files {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			return n, err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64<<10), MaxFileBytes+1)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var e event.Event
			if json.Unmarshal(line, &e) != nil || e.Kind == "" {
				continue
			}
			if err := fn(e); err != nil {
				return n, err
			}
			n++
		}
		if err := os.Remove(p); err != nil {
			return n, err
		}
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	return n, nil
}
