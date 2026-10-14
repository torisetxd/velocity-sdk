package debugtrace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"velocitylabs.cc/sdk"
)

var (
	initOnce sync.Once
	writeMu  sync.Mutex
	sink     io.Writer = io.Discard
	rawSink  io.Writer = io.Discard
	sequence uint64
	streamID uint64
)

func NextID() string {
	return fmt.Sprintf("stream-%06d", atomic.AddUint64(&streamID, 1))
}

func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Log(event string, fields map[string]any) {
	ensureSinks()
	if sink == io.Discard {
		return
	}

	record := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"seq":   atomic.AddUint64(&sequence, 1),
		"event": event,
		"pid":   os.Getpid(),
	}
	for key, value := range fields {
		record[key] = value
	}
	writeRecord(sink, record)
}

func Raw(event string, fields map[string]any, payload string) {
	ensureSinks()
	if rawSink == io.Discard {
		return
	}

	record := map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"seq":     atomic.AddUint64(&sequence, 1),
		"event":   event,
		"pid":     os.Getpid(),
		"payload": payload,
	}
	for key, value := range fields {
		record[key] = value
	}
	writeRecord(rawSink, record)
}

func ensureSinks() {
	initOnce.Do(func() {
		settings := sdk.Current()
		sink = openSink(settings.DebugValue(os.Getenv, ""))
		rawSink = openSink(settings.DebugValue(os.Getenv, "_RAW"))
	})
}

func openSink(raw string) io.Writer {
	target := strings.TrimSpace(raw)
	if target == "" || target == "0" || strings.EqualFold(target, "off") || strings.EqualFold(target, "false") {
		return io.Discard
	}
	if target == "-" || strings.EqualFold(target, "stderr") {
		return os.Stderr
	}

	file, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: open %s: %v\n", sdk.Current().DebugEnv, target, err)
		return io.Discard
	}
	return file
}

func writeRecord(target io.Writer, record map[string]any) {
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = json.NewEncoder(target).Encode(record)
}
