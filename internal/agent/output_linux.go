//go:build linux

package agent

import (
	"bufio"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"toft/internal/runner"
)

// A workload's output: the runner's stdout and stderr, each a FIFO the agent
// reads, appended to one file the platform reads back (ADR §137).
//
// **FIFOs rather than pipes**, so output survives the agent. A pipe's reader
// dies with the agent, and a runner writing to it next would be killed by
// SIGPIPE — taking the app with the agent it never needed. A FIFO is a name:
// the agent that replaces this one opens it again and the runner's descriptor
// is still the same channel. The runner ignores SIGPIPE, so a line written
// while no agent is reading is lost and nothing else is.
//
// The file is the agent's alone to write and to rotate. Every line in it is
// marked — by the runner, which stamps everything it emits, or by the agent
// for the rare line it did not (a runner that panicked before its logger was
// set) — so runner.ParseLogLine is its one reader, in both execution modes.

// RotateBytes is the size past which a workload's log is rotated: kept to one
// earlier generation, so a workload holds at most twice this. A var so a test
// can rotate in a few lines.
var RotateBytes int64 = 4 << 20

const (
	maxLine      = 16 << 10
	truncatedTag = " …[truncated]"
	subBuffer    = 4096
)

type output struct {
	name, appID string
	labels      map[string]string
	fifoOut     string
	fifoErr     string
	logPath     string
	publish     func(Event)

	mu      sync.Mutex
	file    *os.File
	size    int64
	readers []*os.File
	closed  bool
	wg      sync.WaitGroup
}

// openOutput makes the workload's FIFOs (or finds them), starts reading both,
// and opens its log for appending.
func (a *Agent) openOutput(spec Spec) (*output, error) {
	o := &output{
		name: spec.Name, appID: spec.AppID, labels: spec.Labels,
		fifoOut: filepath.Join(a.cfg.Dir, "out", spec.Name+".out"),
		fifoErr: filepath.Join(a.cfg.Dir, "out", spec.Name+".err"),
		logPath: spec.LogPath,
		publish: a.publish,
	}
	if err := o.openLog(); err != nil {
		return nil, err
	}
	for stream, path := range map[string]string{"stdout": o.fifoOut, "stderr": o.fifoErr} {
		if err := ensureFIFO(path); err != nil {
			o.close()
			return nil, err
		}
		// Read-write, so the agent itself is always a writer: a read never sees
		// the end of the stream when one runner exits and the next has not yet
		// opened it, and opening it to write never blocks.
		r, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			o.close()
			return nil, err
		}
		o.readers = append(o.readers, r)
		o.wg.Add(1)
		go o.read(stream, r)
	}
	return o, nil
}

func ensureFIFO(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o600)
}

// writers opens the two FIFOs for one process to write.
//
// Opened with a bare open(2), never os.OpenFile: Go puts a FIFO it opens into
// non-blocking mode for its poller, the flag belongs to the open file the child
// inherits, and a runner whose stdout is non-blocking gets EAGAIN where it
// should wait for the agent to catch up.
func (o *output) writers() (stdout, stderr *os.File, err error) {
	open := func(path string) (*os.File, error) {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	if stdout, err = open(o.fifoOut); err != nil {
		return nil, nil, err
	}
	if stderr, err = open(o.fifoErr); err != nil {
		stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func (o *output) openLog() error {
	if o.logPath == "" {
		return nil
	}
	f, err := os.OpenFile(o.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	o.file, o.size = f, info.Size()
	return nil
}

// read is one FIFO's loop: a line at a time, the longest kept whole up to
// maxLine and truncated past it.
func (o *output) read(stream string, r *os.File) {
	defer o.wg.Done()
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			complete := chunk[len(chunk)-1] == '\n'
			if complete {
				chunk = chunk[:len(chunk)-1]
			}
			if !over {
				room := maxLine - len(line)
				if len(chunk) > room {
					line = append(line, chunk[:room]...)
					over = true
				} else {
					line = append(line, chunk...)
				}
			}
			if complete {
				text := string(line)
				if over {
					text += truncatedTag
				}
				o.append(stream, text)
				line, over = line[:0], false
			}
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if len(line) > 0 {
				o.append(stream, string(line))
			}
			return
		}
	}
}

// append writes one line to the log, marked, and hands it to followers.
func (o *output) append(stream, text string) {
	if at, _, _, _ := runner.ParseLogLine(text); at.IsZero() {
		// Unmarked: the runner did not say it, so it is the runner's own raw
		// output and the platform's voice — never the app's, which only ever
		// reaches the runner's descriptors marked.
		text = runner.LogMarker(time.Now(), runner.LogSourcePlatform, stream) + text
	}
	o.mu.Lock()
	if o.file != nil && !o.closed {
		n, _ := io.WriteString(o.file, text+"\n")
		o.size += int64(n)
		if o.size >= RotateBytes {
			o.rotate()
		}
	}
	o.mu.Unlock()
	o.publish(Event{Type: EventLine, Workload: o.name, AppID: o.appID, Labels: o.labels, Line: text})
}

// rotate moves the live file aside as the one earlier generation. Called with
// o.mu held. A failure keeps writing where it was: a log may never stop a
// workload.
func (o *output) rotate() {
	if err := os.Rename(o.logPath, o.logPath+".1"); err != nil {
		logf("%s: rotating its log: %v", o.name, err)
		return
	}
	o.file.Close()
	o.file = nil
	if err := o.openLog(); err != nil {
		logf("%s: reopening its log after rotation: %v", o.name, err)
	}
}

// close stops reading and closes the log. The FIFOs stay, for a replacement.
func (o *output) close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	for _, r := range o.readers {
		r.Close()
	}
	o.mu.Unlock()
	o.wg.Wait()
	o.mu.Lock()
	if o.file != nil {
		o.file.Close()
		o.file = nil
	}
	o.mu.Unlock()
}

// drain waits, briefly, for what the exited process left in its FIFOs to be
// read: once no writer but the agent holds them, the bytes still in each pipe
// are all there will be, and FIONREAD (TIOCINQ) says when the readers have taken them.
func (o *output) drain(limit time.Duration) {
	if o == nil {
		return
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		pending := 0
		for _, r := range o.readers {
			// Through SyscallConn, never Fd: Fd puts the descriptor in
			// blocking mode, and a blocked read is one Close cannot end.
			if rc, err := r.SyscallConn(); err == nil {
				_ = rc.Control(func(fd uintptr) {
					if n, err := unix.IoctlGetInt(int(fd), unix.TIOCINQ); err == nil {
						pending += n
					}
				})
			}
		}
		o.mu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// What a reader has taken but not yet written is one buffered line away.
	time.Sleep(50 * time.Millisecond)
}

// isOpen says whether the output still holds its log file.
func (o *output) isOpen() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.file != nil
}

func (o *output) removeFIFOs() {
	if o == nil {
		return
	}
	_ = os.Remove(o.fifoOut)
	_ = os.Remove(o.fifoErr)
}

// removeFiles removes the log and its earlier generation, which only a delete
// does: a restart's previous lines are what explain it.
func (o *output) removeFiles() {
	if o == nil || o.logPath == "" {
		return
	}
	_ = os.Remove(o.logPath)
	_ = os.Remove(o.logPath + ".1")
}

// subscriber is one follower: of one workload, or of all of them.
type subscriber struct {
	workload string
	ch       chan Event
	// dropped counts events a full buffer refused since the last one that
	// fitted, which is said to the follower as soon as one fits again.
	dropped int
}

// publish hands an event to every follower that wants it. A follower a buffer
// behind loses events rather than holding up the workload's output, and is
// told how many: the stream is never ended over it, because the platform's
// copy of every app's output is one such follower.
func (a *Agent) publish(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for s := range a.subs {
		if s.workload != "" && s.workload != e.Workload {
			continue
		}
		if s.dropped > 0 {
			select {
			case s.ch <- Event{Type: EventDropped, Workload: e.Workload, AppID: e.AppID, Dropped: s.dropped}:
				s.dropped = 0
			default:
				s.dropped++
				continue
			}
		}
		select {
		case s.ch <- e:
		default:
			s.dropped++
		}
	}
}

func (a *Agent) subscribe(workload string) *subscriber {
	s := &subscriber{workload: workload, ch: make(chan Event, subBuffer)}
	a.mu.Lock()
	a.subs[s] = struct{}{}
	a.mu.Unlock()
	return s
}

func (a *Agent) unsubscribe(s *subscriber) {
	a.mu.Lock()
	if _, ok := a.subs[s]; ok {
		delete(a.subs, s)
		close(s.ch)
	}
	a.mu.Unlock()
}
