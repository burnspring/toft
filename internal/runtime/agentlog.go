package runtime

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"toft/internal/agent"
)

// The agent's log is a file in AGENT_DIR, which on a container install only an
// exec into the container can read: on trytoft it was where the one record of
// what the agent did sat while every diagnosis was made from the database. So
// the platform follows it and says each new line again in its own log — the
// journal's, the cloud log group's — with the agent's level and fields kept and
// `source=agent` added. The agent outlives the platform, so it cannot write to
// the platform's stdout itself; the file is the hand-off.

// FollowAgentLog re-logs the agent's new lines until ctx ends. It starts at the
// file's end — what was written before this platform started was said by the
// platform before it — and survives the agent's rotation (agent.LogMaxBytes).
func (r *AgentRuntime) FollowAgentLog(ctx context.Context) {
	go followAgentLog(ctx, agent.LogPath(r.client.Dir()), slog.Default(), time.Second)
}

func followAgentLog(ctx context.Context, path string, out *slog.Logger, every time.Duration) {
	var (
		f   *os.File
		br  *bufio.Reader
		ino uint64
	)
	open := func(fromEnd bool) {
		nf, err := os.Open(path)
		if err != nil {
			return
		}
		if fromEnd {
			if _, err := nf.Seek(0, io.SeekEnd); err != nil {
				nf.Close()
				return
			}
		}
		if f != nil {
			f.Close()
		}
		f, br, ino = nf, bufio.NewReader(nf), inode(nf)
	}
	open(true)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var partial string
	for {
		if f != nil {
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					// An unterminated line is the agent mid-write: kept until
					// its newline arrives.
					partial += line
					break
				}
				relogAgentLine(ctx, out, strings.TrimRight(partial+line, "\n"))
				partial = ""
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		// Rotated (a new file at the path) or never opened: the new file is
		// read from its start, since everything in it is new.
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if f == nil {
			open(false)
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino != ino {
			// What the old file gained before it was renamed is read first.
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					break
				}
				relogAgentLine(ctx, out, strings.TrimRight(partial+line, "\n"))
				partial = ""
			}
			partial = ""
			open(false)
		}
	}
}

func inode(f *os.File) uint64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// relogAgentLine says one agent line in the platform's log. A line in the
// agent's logfmt keeps its level, message and fields; anything else — a panic,
// the Go runtime's own output — is said whole, as a warning.
func relogAgentLine(ctx context.Context, out *slog.Logger, line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	pairs, ok := parseLogfmt(line)
	if !ok {
		out.Warn("agent: "+line, "source", "agent")
		return
	}
	level, msg := slog.LevelInfo, ""
	attrs := []any{"source", "agent"}
	for _, kv := range pairs {
		switch kv[0] {
		case "time":
			// The platform stamps its own; the agent's is a moment earlier.
		case "level":
			_ = level.UnmarshalText([]byte(kv[1]))
		case "msg":
			msg = kv[1]
		default:
			attrs = append(attrs, kv[0], kv[1])
		}
	}
	out.Log(ctx, level, "agent: "+msg, attrs...)
}

// parseLogfmt reads slog's text format: key=value pairs, a value quoted when it
// holds a space, a quote or an equals sign. A line that does not start with a
// `time=` pair is not the agent's structured output.
func parseLogfmt(line string) ([][2]string, bool) {
	if !strings.HasPrefix(line, "time=") {
		return nil, false
	}
	var out [][2]string
	rest := line
	for rest != "" {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 || strings.ContainsAny(rest[:eq], " \"") {
			return nil, false
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return nil, false
			}
			val, _ = strconv.Unquote(q)
			rest = rest[len(q):]
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			val, rest = rest[:sp], rest[sp:]
		} else {
			val, rest = rest, ""
		}
		out = append(out, [2]string{key, val})
	}
	return out, true
}
