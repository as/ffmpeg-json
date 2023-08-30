package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/as/log"
)

var (
	split = strings.Split
	trim  = strings.TrimSpace
)

func watchState(r io.Reader, state chan<- State) {
	defer close(state)
	sc := bufio.NewScanner(CRtoLF{r}) // util.go:/CRtoLF/
	s0 := State{}
	for sc.Scan() {
		hastext := func(s string) bool {
			return strings.Contains(sc.Text(), s)
		}
		// NOTE(as): HWFRAMES3
		// Self-explanitory string check. That's it.
		if hastext("No decoder surfaces left") {
			hwframesbug = true
		}
		// NOTE(as): gpu out of memory
		if hastext("CUDA_ERROR_OUT_OF_MEMORY") || (hastext("nvenc") && hastext("OpenEncodeSessionEx failed")) {
			vramoverflow = true
		}

		log.Debug.F("watch: state: %v", sc.Text())
		s1 := State{}.Decode(sc.Text())
		if s1.Frame <= s0.Frame && s1.Size <= s0.Size {
			continue
		}
		state <- s1
		s0 = s1
	}
}

// State is a carriage-return delimited output line in ffmpeg
type State struct {
	Frame   int
	FPS     int
	Q       float64
	Time    Time
	Size    int
	Bitrate float64
	Dup     int
	Drop    int
	Speed   float64
}

func (s State) Fields() (kv []any) {
	return []interface{}{
		"frame", s.Frame,
		"runtime", s.Time.Duration().Seconds(),
		"size", 1024 * s.Size,
		"dup", s.Dup,
		"drop", s.Drop,
		"bps", int(1000 * s.Bitrate),
		"fps", s.FPS,
		"speed", fmt.Sprintf("%0.2f", s.Speed),
		"q", s.Q,
	}
}

// Progress returns a value between [0, 1] inclusive
func (s State) Progress(max time.Duration, frames int) float64 {
	if max != 0 {
		return s.Time.Duration().Seconds() / max.Seconds()
	}
	return float64(s.Frame) / float64(frames)
}

// Decode decodes line into a new state and returns it. The line
// must begin with "frame=" (video) or "size=" (audio, packaging)
// which is what the state line looks like in the ffmpeg output.
func (s State) Decode(line string) State {
	if !strings.HasPrefix(line, "frame=") && !strings.HasPrefix(line, "size=") {
		return s
	}
	symtab := map[string]interface{}{
		"frame":   &s.Frame,
		"fps":     &s.FPS,
		"size":    &s.Size,
		"time":    &s.Time,
		"Lsize":   &s.Size, // ffmpeg bug?
		"bitrate": &s.Bitrate,
		"dup":     &s.Dup,
		"drop":    &s.Drop,
		"q":       &s.Q,
		"speed":   &s.Speed,
	}

	// ffmpeg formatting is left-padded for numbers
	// so get rid of the equal signs and treat the input
	// as a space seperated list
	a := split(demangle(line), " ")

	// scan each keypair into the symbol table
	for i := 1; i < len(a); i += 2 {
		dst, ok := symtab[trim(a[i-1])]
		if ok {
			fmt.Sscan(trim(a[i]), dst)
		}
	}
	s.FPS *= (targetOutputs)
	s.Speed *= round100(float64(targetOutputs))
	return s
}

// demangle splits the line into space-seperated
// values, discarding equal signs from the input.
func demangle(line string) (s string) {
	sep := ""
	for _, v := range split(line, "=") {
		s += sep + trim(v)
		sep = " "
	}
	return s
}

// Time helps us parse ffmpeg log times
type Time string

func (t Time) Duration() time.Duration {
	var h, m, s float64
	fmt.Sscanf(string(t), "%f:%f:%f", &h, &m, &s)
	return floatDur(3600*h + 60*m + s)
}

// CRtoLF replaces all carriage returns with line feeds
type CRtoLF struct {
	io.Reader
}

func (c CRtoLF) Read(p []byte) (n int, err error) {
	n, err = c.Reader.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\r' {
			p[i] = '\n'
		}
	}
	return
}
