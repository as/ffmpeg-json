// ffmpeg-json
// run this like you with a regular ffmpeg command
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/as/log"
)

var (
	// save original ffmpeg standard error stream to this named file.
	// default: temp file
	stderr = os.Getenv("STDERR")

	// maxstall aborts the process if encoded frame count increases
	// past zero and then stalls for maxstall intervals. This usually
	// happens when ffmpeg is used with an unreliable http source
	maxstall, _ = strconv.Atoi(os.Getenv("MAXSTALL"))

	// logFreq outputs logs at the given frequency in seconds
	// default=3.0
	logFreq = stringDur(os.Getenv("LOGFREQ"))

	// maxdup, if non-zero, terminates the process with an error
	// if maxdup duplicate frames are detected during transcoding
	maxdup, _ = strconv.Atoi(os.Getenv("MAXDUP"))

	// targetDur, if non-zero, calculates structured progress output
	// based on the encoder output timestamps
	targetDur = stringDur(os.Getenv("DUR"))

	// targetFrames, if non-zero, calculates structured progress output
	// based on the expected number of frames encoded
	targetFrames, _ = strconv.Atoi(os.Getenv("FRAMES"))

	// targetOutputs, if non-zero, adjusts FPS and SPEED with a
	// multiplier
	targetOutputs, _ = strconv.Atoi(os.Getenv("OUTPUTS"))
)

func init() {
	if maxstall == 0 {
		maxstall = 1000
	}
	if logFreq == 0 {
		logFreq = 3 * time.Second
	}
	if targetOutputs == 0 {
		targetOutputs++
	}
}

func main() {
	defer log.Trap()
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		log.Fatal.F("ffmpeg not found: %v", err)
	}

	fd2 := os.Stderr
	if stderr == "" {
		fd2, err = os.CreateTemp("", "ffmpeg")
	} else {
		fd2, err = os.Create(stderr)
	}
	if fd2 == nil {
		log.Error.F("failed to open stderr file, using default stream")
		fd2 = os.Stderr
	}

	statr, statw := biopipe()

	donec := make(chan error) // command execution channel
	ctx, kill := context.WithCancel(context.Background())
	defer kill()

	// run the command
	// inherit from parent process and override
	// necessary values.
	go func() {
		//fd2 = os.Stderr
		donec <- ffmpeg(ctx, io.MultiWriter(fd2, statw), os.Args[1:]...)
		statw.Close()
	}()

	statc := make(chan State, 10) // status channel
	go watchState(statr, statc)

	update := time.NewTicker(logFreq)
	defer update.Stop()
	prior := State{}
	nstall := 0
	log.Info.Add("topic", "status", "action", "update", "progress", progress(prior)).Add(prior.Fields()...).Printf("")
	for statc != nil {
		select {
		case err := <-donec:
			if err == nil {
				log.Info.Add("topic", "summary", "action", "done", "progress", 100).Add(prior.Fields()...).Printf("done")
			} else {
				fd2.Seek(0, 0)
				logdata := new(bytes.Buffer)
				io.Copy(logdata, fd2)
				log.Fatal.Add("topic", "summary", "action", "done", "err", err, "progress", -100).Printf("failed: %q", lastline(logdata))
			}
		case current, more := <-statc:
			if !more {
				statc = nil
				continue
			}
			if maxdup > 0 && current.Dup >= maxdup {
				kill()
				log.Fatal.Add("topic", "dup", "frames", current.Dup, "limit", maxdup, "fatal", true).Printf("freeze detected")
			}
			if current.Frame <= prior.Frame && current.Frame != 0 {
				nstall++
			} else {
				nstall = 0
			}
			prior = current
			if maxstall > 0 && nstall > maxstall {
				kill()
				log.Fatal.Add("topic", "status", "action", "stall", "frame", current.Frame).Printf("stalled on frame %d after %d updates", current.Frame, nstall)
			}
		case <-update.C:
			log.Info.Add("topic", "status", "action", "update", "progress", progress(prior)).Add(prior.Fields()...).Printf("")
		}
	}
}

func ffmpeg(ctx context.Context, stderr io.Writer, args ...string) (err error) {
	ln := log.Info.Add("topic", "transcode")
	ln.Add("action", "start").Printf("cmd: ffmpeg %q", args)
	defer ln.Add("action", "stop", "err", err).Printf("")

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	r, _ := cmd.StderrPipe()
	if err = cmd.Start(); err != nil {
		return
	}
	if _, err = io.Copy(stderr, bufio.NewReader(r)); err != nil {
		return
	}
	return cmd.Wait()
}

var errLine = regexp.MustCompile("^[eE]rror")

func lastline(r io.Reader) (msg string) {
	sc := bufio.NewScanner(r)
	sep := ""
	for sc.Scan() {
		line := sc.Text()
		if errLine.MatchString(line) {
			msg = sep + line
			sep = ", "
		}
	}
	return
}

func biopipe() (io.Reader, io.WriteCloser) {
	r, w := io.Pipe()
	return bufio.NewReader(r), w
}

func round100(f float64) float64 {
	return math.Round(f*100) / 100
}
func progress(current State) (perc int) {
	perc = int(current.Progress(targetDur, targetFrames) * 100)
	if perc < 0 {
		return 0
	}
	return
}
func stringDur(s string) time.Duration {
	dur, _ := time.ParseDuration(fmt.Sprintf("%ss", s))
	return dur
}
func floatDur(f float64) time.Duration {
	dur, _ := time.ParseDuration(fmt.Sprintf("%fs", f))
	return dur
}
