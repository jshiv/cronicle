package cronicle

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	log "github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// LogFormat selects the stdout log rendering.
type LogFormat string

const (
	LogFormatAuto   LogFormat = "auto"
	LogFormatPretty LogFormat = "pretty"
	LogFormatText   LogFormat = "text"
	LogFormatJSON   LogFormat = "json"
)

// SetupLogging configures logrus's stdout formatter according to format.
// "auto" picks pretty when stdout is a TTY, text otherwise.
func SetupLogging(format LogFormat) {
	resolved := format
	if resolved == "" || resolved == LogFormatAuto {
		if isatty.IsTerminal(os.Stdout.Fd()) {
			resolved = LogFormatPretty
		} else {
			resolved = LogFormatText
		}
	}

	switch resolved {
	case LogFormatPretty:
		log.SetFormatter(&prettyFormatter{
			fallback: &log.TextFormatter{
				FullTimestamp: true,
				ForceColors:   true,
			},
		})
	case LogFormatJSON:
		log.SetFormatter(&log.JSONFormatter{})
	default:
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}

	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)
}

// EnableFileLog adds a logrus hook that mirrors every log entry as JSON to
// croniclePath/.cronicle/log/cronicle.jsonl, rotated by lumberjack.
// Stdout output is unaffected.
func EnableFileLog(croniclePath string) error {
	logDir := filepath.Join(croniclePath, ".cronicle", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	hook := &jsonFileHook{
		writer: &lumberjack.Logger{
			Filename:   filepath.Join(logDir, "cronicle.jsonl"),
			MaxSize:    500, // MB per file before rotation
			MaxBackups: 3,   // keep up to 3 rotated files
			MaxAge:     28,  // days; older files deleted
			Compress:   true,
		},
		formatter: &log.JSONFormatter{},
	}
	log.AddHook(hook)
	return nil
}

// jsonFileHook writes every log entry as JSON to an io.Writer (typically a
// rotating lumberjack logger). Stdout is untouched, so this hook composes with
// any stdout formatter.
type jsonFileHook struct {
	writer    io.Writer
	formatter log.Formatter
}

func (h *jsonFileHook) Levels() []log.Level {
	return log.AllLevels
}

func (h *jsonFileHook) Fire(e *log.Entry) error {
	b, err := h.formatter.Format(e)
	if err != nil {
		return err
	}
	_, err = h.writer.Write(b)
	return err
}

// TZFormatter enables timezone specifc logrus formatting
// Example:
// loc, _ = time.LoadLocation("America/Los_Angeles")
// log.SetFormatter(TZFormatter{Formatter: &log.TextFormatter{
//
//	FullTimestamp: true,
//	}, loc: loc})
type TZFormatter struct {
	log.Formatter
	loc *time.Location
}

// Format sets the timezone for the given loc *time.Timezone
func (u TZFormatter) Format(e *log.Entry) ([]byte, error) {
	e.Time = e.Time.In(u.loc)
	return u.Formatter.Format(e)
}

// ApplyTimezone wraps the standard logger's current formatter in a TZFormatter
// so timestamps render in loc. This preserves the user's --log-format choice
// instead of clobbering it with a fresh TextFormatter.
func ApplyTimezone(loc *time.Location) {
	current := log.StandardLogger().Formatter
	if _, alreadyTZ := current.(TZFormatter); alreadyTZ {
		return
	}
	log.SetFormatter(TZFormatter{Formatter: current, loc: loc})
}

// prettyFormatter renders agent_run entries as a multi-line block and falls
// back to a wrapped TextFormatter for everything else.
type prettyFormatter struct {
	fallback log.Formatter
}

func (p *prettyFormatter) Format(e *log.Entry) ([]byte, error) {
	entryType, _ := e.Data["entry_type"].(string)
	if entryType != "agent_run" {
		return p.fallback.Format(e)
	}
	return p.renderAgentRun(e), nil
}

func (p *prettyFormatter) renderAgentRun(e *log.Entry) []byte {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()
	footer := color.New(color.Faint).SprintFunc()
	errc := color.New(color.FgRed, color.Bold).SprintFunc()

	schedule, _ := e.Data["schedule"].(string)
	task, _ := e.Data["task"].(string)
	model, _ := e.Data["model"].(string)
	response, _ := e.Data["response"].(string)
	costStr, _ := e.Data["cost_usd"].(string)
	durationMs := asInt64(e.Data["duration_ms"])
	in := asInt64(e.Data["input_tokens"])
	out := asInt64(e.Data["output_tokens"])
	transcript, _ := e.Data["transcript"].(string)
	stop, _ := e.Data["stop_reason"].(string)
	errMsg, _ := e.Data["error"].(string)
	success, _ := e.Data["success"].(bool)

	headerLine := fmt.Sprintf("agent run · schedule=%s · task=%s · model=%s",
		schedule, task, model)
	bar := strings.Repeat("━", lenWithoutANSI(headerLine)+8)

	var b bytes.Buffer
	b.WriteString(rule(bar))
	b.WriteString("\n")
	b.WriteString(header(headerLine))
	b.WriteString("\n")
	b.WriteString(rule(bar))
	b.WriteString("\n\n")

	if success {
		if response == "" {
			b.WriteString(footer("(no text response)\n"))
		} else {
			b.WriteString(strings.TrimRight(response, "\n"))
			b.WriteString("\n")
		}
	} else {
		b.WriteString(errc("ERROR: "))
		b.WriteString(errMsg)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	footerParts := []string{
		fmt.Sprintf("%d in / %d out tokens", in, out),
		fmt.Sprintf("$%s", costStr),
		fmt.Sprintf("%dms", durationMs),
	}
	if stop != "" {
		footerParts = append(footerParts, fmt.Sprintf("stop=%s", stop))
	}
	if transcript != "" {
		footerParts = append(footerParts, fmt.Sprintf("transcript=%s", filepath.Base(transcript)))
	}
	b.WriteString(footer("[" + strings.Join(footerParts, " · ") + "]"))
	b.WriteString("\n\n")

	return b.Bytes()
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// lenWithoutANSI counts visible runes, ignoring ANSI escape codes. Used so the
// rule line above the header matches its visible width when colors are on.
func lenWithoutANSI(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		n++
	}
	return n
}
