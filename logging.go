package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// consoleHandler keeps structured fields but prints error details as readable
// multiline text rather than escaped strings.
type consoleHandler struct {
	output io.Writer
	mu     *sync.Mutex
	level  slog.Level
	attrs  []slog.Attr
	group  string
}

func newLogger(output io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&consoleHandler{
		output: output,
		mu:     &sync.Mutex{},
		level:  level,
	})
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	var details []string
	fmt.Fprintf(&line, "%s %s", record.Level, record.Message)

	var appendAttr func(string, slog.Attr)
	appendAttr = func(prefix string, attr slog.Attr) {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			return
		}

		key := prefix + attr.Key
		if attr.Value.Kind() == slog.KindGroup {
			if attr.Key != "" {
				key += "."
			}

			for _, child := range attr.Value.Group() {
				appendAttr(key, child)
			}

			return
		}

		if err, ok := attr.Value.Any().(error); ok {
			details = append(details, err.Error())
			return
		}

		fmt.Fprintf(&line, " %s=%s", key, attr.Value)
	}

	for _, attr := range h.attrs {
		appendAttr("", attr)
	}

	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(h.group, attr)
		return true
	})

	for _, detail := range details {
		line.WriteString("\n  ")
		detail = strings.ReplaceAll(detail, "\r\n", "\n")
		detail = strings.ReplaceAll(detail, "\r", "\n")
		line.WriteString(strings.ReplaceAll(detail, "\n", "\n  "))
	}

	line.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := io.WriteString(h.output, line.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append([]slog.Attr(nil), h.attrs...)

	if h.group != "" {
		attrs = []slog.Attr{{Value: slog.GroupValue(attrs...), Key: strings.TrimSuffix(h.group, ".")}}
	}

	next.attrs = append(next.attrs, attrs...)
	return &next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	next := *h
	if name != "" {
		next.group += name + "."
	}

	return &next
}
