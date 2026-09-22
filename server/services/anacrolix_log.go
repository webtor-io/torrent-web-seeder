package services

import (
	"context"
	"log/slog"

	log "github.com/sirupsen/logrus"
)

// anacrolixLogHandler forwards the torrent library's structured log records
// to logrus. Only warnings and errors pass: the library is chatty at Info and
// Debug, and those levels stay with its own (discarded) logger.
//
// Why this exists: the library's reader logs the reason a storage read
// failed ("initial read failed", "read failed after reader reset") and only
// THEN, on the third failure, panics inside updatePieceCompletion with the
// bare message "0 N". With the library logger discarded (torrent_client.go)
// only the panic reached the logs, so 42 recovered panics on 2026-09-22 had
// no visible cause. Attributes become logrus fields; groups are flattened
// with dots so the anacrolix "torrent.ih" group shows up as one field.
type anacrolixLogHandler struct {
	min    slog.Level
	fields log.Fields
	prefix string
}

func newAnacrolixLogHandler(min slog.Level) *anacrolixLogHandler {
	return &anacrolixLogHandler{min: min, fields: log.Fields{}}
}

func (h *anacrolixLogHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.min
}

func (h *anacrolixLogHandler) Handle(_ context.Context, r slog.Record) error {
	fields := make(log.Fields, len(h.fields)+r.NumAttrs()+1)
	for k, v := range h.fields {
		fields[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		h.addAttr(fields, h.prefix, a)
		return true
	})
	fields["source"] = "anacrolix"
	entry := log.WithFields(fields)
	switch {
	case r.Level >= slog.LevelError:
		entry.Error(r.Message)
	case r.Level >= slog.LevelWarn:
		entry.Warn(r.Message)
	default:
		entry.Info(r.Message)
	}
	return nil
}

func (h *anacrolixLogHandler) addAttr(fields log.Fields, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			h.addAttr(fields, p, ga)
		}
		return
	}
	if a.Key == "" {
		return
	}
	fields[prefix+a.Key] = a.Value.Any()
}

func (h *anacrolixLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := &anacrolixLogHandler{min: h.min, fields: make(log.Fields, len(h.fields)+len(attrs)), prefix: h.prefix}
	for k, v := range h.fields {
		nh.fields[k] = v
	}
	for _, a := range attrs {
		h.addAttr(nh.fields, h.prefix, a)
	}
	return nh
}

func (h *anacrolixLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &anacrolixLogHandler{min: h.min, fields: h.fields, prefix: h.prefix + name + "."}
}
