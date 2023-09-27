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

	retry, _    = strconv.Atoi(os.Getenv("RETRY"))
	maxretry, _ = strconv.Atoi(os.Getenv("MAXRETRY"))
)

// NOTE(as): HWFRAMES: We might need to re-execute ffmpeg with a new value for extra_hw_frames
// Search for HWFRAMES1 for notes
var (
	hwframesbug    = false
	hwframesptr    *string
	hwframes       = 0
	hwframesmax, _ = strconv.Atoi(os.Getenv("MAXEXTRAHWFRAMES"))

	vramoverflow = false
)

func init() {
	if hwframesmax == 0 {
		hwframesmax = 64
	}
	if maxstall == 0 {
		maxstall = 1000
	}
	if logFreq == 0 {
		logFreq = 3 * time.Second
	}
	if targetOutputs == 0 {
		targetOutputs++
	}
	if maxretry == 0 {
		maxretry = 60
	}
}

var procstart = time.Now()

func main() {
	log.DebugOn = false

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

	// NOTE(as): HWFRAMES1: For GPU featuresets, scan for hwframes on the command line and keep track of it
	// because this value might be too small or too large for some media. In our case, assume its always too small
	// and increment it with retry as a brute force solution for now. See HWFRAMES2
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i-1] == "-extra_hw_frames" {
			hwframesptr = &os.Args[i]
			hwframes, _ = strconv.Atoi(*hwframesptr)
			log.Info.Add("topic", "gpu", "action", "bootstrap", "extra_hw_frames", hwframes).Printf("detected -extra_hw_frames arg")
		}
	}

	// run the command
	// inherit from parent process and override
	// necessary values.
	go func() {
		//fd2 = os.Stderr
		donec <- ffmpeg(ctx, io.MultiWriter(fd2, statw), os.Args[1:]...)
		statw.Close()
	}()

	statc := make(chan State, 1000) // status channel
	go watchState(statr, statc)

	update := time.NewTicker(logFreq)
	defer update.Stop()
	prior := State{}
	nstall := 0
	log.Info.Add("topic", "status", "action", "update", "progress", progress(prior)).Add(prior.Fields()...).Printf("")
	for statc != nil {
		select {
		case err := <-donec:
			fd2.Seek(0, 0)
			logdata := new(bytes.Buffer)
			io.Copy(logdata, fd2)

			lasterr := lastline(logdata)
			if err == nil && lasterr != "" {
				err = fmt.Errorf("ffmpeg failed")
			}
			if err == nil {
				log.Info.Add("topic", "summary", "action", "done", "progress", 100, "uptime", time.Since(procstart).Seconds()).Add(prior.Fields()...).Printf("done")
			} else {
				doretry := func() {
					c := exec.Command(os.Args[0], os.Args[1:]...)
					c.Stdin = os.Stdin
					c.Stdout = os.Stdout
					c.Stderr = os.Stderr
					retry++
					c.Env = append([]string{}, os.Environ()...)
					c.Env = append(c.Env, fmt.Sprintf("RETRY=%d", retry))
					err := c.Run()
					if err != nil {
						os.Exit(1)
					}
					os.Exit(0)
				}

				if vramoverflow {
					ln := log.Error.Add(
						"topic", "gpu", "action", "oom", "alert", "gpu note out of vram",
						"retry", retry, "maxretry", maxretry, "err", err,
					)
					if retry >= maxretry {
						ln.Fatal().Printf("max retry reached: gpu OOM: %q", lasterr)
					}
					ln.Printf("retry: gpu OOM: %q", lasterr)
					time.Sleep(2 * time.Second)
					doretry()
				}
				if hwframesbug && hwframes < hwframesmax {
					// NOTE(as): HWFRAMES2
					// This is a dirty hack to restart the process created out of necessity. The argument is incremented and ffmpeg-json
					// re-executes itself. This clobbers all state in the current process, but we haven't done much work anyway.
					//
					// Finally, see state.go:/HWFRAMES3/ for the detection logic
					hwframes++
					*hwframesptr = fmt.Sprint(hwframes)
					log.Error.Add("topic", "gpu", "action", "retry", "extra_hw_frames", hwframes).Printf("increment extra_hw_frames and retry")
					doretry()
				}
				log.Fatal.Add("topic", "summary", "action", "failed", "err", err, "progress", -100).Printf("failed: %q", lasterr)
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

var (
	errLine       = regexp.MustCompile("^[eE]rror")
	errImpossible = regexp.MustCompile("Impossible to open.+")
	errInvalid    = regexp.MustCompile(".+Invalid data found when processing input")
	errNoStream   = regexp.MustCompile("^[Ss]tream map.+matches no stream")

	errCk = []*regexp.Regexp{errLine, errImpossible, errInvalid, errNoStream}
)

func lastline(r io.Reader) (msg string) {
	sc := bufio.NewScanner(r)
	sep := ""
	for sc.Scan() {
		line := sc.Text()
		for _, ck := range errCk {
			if ck.MatchString(line) {
				msg = sep + line
				sep = ", "
				return
			}
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
