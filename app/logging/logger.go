package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	// Adjusted import path to match your project structure
	"github.com/cloudresty/corridor/config"
)

// Level type for log levels
type Level int

// Log levels
const (
	// SilentLevel is the level at which no logs are output.
	SilentLevel Level = iota
	// ErrorLevel logs errors that are fatal to the operation, but not the service or application.
	ErrorLevel
	// WarnLevel logs messages about issues that are not critical but should be noted.
	WarnLevel
	// InfoLevel logs general operational messages.
	InfoLevel
	// DebugLevel logs detailed information, typically for debugging.
	DebugLevel
)

var (
	levelToString = map[Level]string{
		ErrorLevel: "ERROR",
		WarnLevel:  "WARN",
		InfoLevel:  "INFO",
		DebugLevel: "DEBUG",
	}

	stringToLevel = map[string]Level{
		"error": ErrorLevel,
		"warn":  WarnLevel,
		"info":  InfoLevel,
		"debug": DebugLevel,
	}
)

// Logger structure
type Logger struct {
	mu        sync.Mutex
	out       io.Writer
	level     Level
	formatter Formatter
	// For including caller info
	reportCaller bool
	callerSkip   int
}

// Formatter interface for log message formatting
type Formatter interface {
	Format(level Level, timestamp time.Time, message string, fields map[string]interface{}, caller string) ([]byte, error)
}

// TextFormatter formats logs as plain text
type TextFormatter struct {
	TimestampFormat string
}

// JSONFormatter formats logs as JSON
type JSONFormatter struct{}

// LogEntry for JSON formatting
type jsonLogEntry struct {
	Timestamp string                 `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Caller    string                 `json:"caller,omitempty"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// Format for TextFormatter
func (f *TextFormatter) Format(level Level, timestamp time.Time, message string, fields map[string]interface{}, caller string) ([]byte, error) {

	var b bytes.Buffer
	tsFormat := f.TimestampFormat
	if tsFormat == "" {
		tsFormat = time.RFC3339
	}

	b.WriteString(timestamp.Format(tsFormat))
	b.WriteString(" [")
	b.WriteString(levelToString[level])
	b.WriteString("] ")

	if caller != "" {
		b.WriteString(caller)
		b.WriteString(" ")
	}

	b.WriteString(message)

	if len(fields) > 0 {

		b.WriteString(" {")
		first := true

		for k, v := range fields {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s: %v", k, v)
			first = false
		}

		b.WriteString("}")

	}

	b.WriteByte('\n')

	return b.Bytes(), nil

}

// Format for JSONFormatter
func (f *JSONFormatter) Format(level Level, timestamp time.Time, message string, fields map[string]interface{}, caller string) ([]byte, error) {

	entry := jsonLogEntry{
		Timestamp: timestamp.UTC().Format(time.RFC3339Nano),
		Level:     levelToString[level],
		Message:   message,
		Caller:    caller,
		Fields:    fields,
	}

	return json.Marshal(entry)

}

var std = New(os.Stdout, InfoLevel, &TextFormatter{TimestampFormat: time.RFC3339}, false)

// New creates a new Logger.
func New(out io.Writer, level Level, formatter Formatter, reportCaller bool) *Logger {

	return &Logger{
		out:          out,
		level:        level,
		formatter:    formatter,
		reportCaller: reportCaller,
		callerSkip:   2, // Default skip: log func -> output func -> runtime.Caller
	}

}

// Init initializes the standard logger based on configuration.
// It configures the package-level `std` logger.
func Init(globalCfg *config.GlobalConfig, pluginLoggingCfg map[string]interface{}) {

	// Lock will be handled by individual setter methods or output method
	std.out = os.Stdout // Default output

	// Set log level
	lvlStr := globalCfg.LogLevel // LogLevel is always present due to config defaulting

	if lvlStr != "" {

		parsedLevel, found := stringToLevel[strings.ToLower(lvlStr)]

		if found {
			std.mu.Lock()
			std.level = parsedLevel
			std.mu.Unlock()

		} else {

			log.Printf("logging: Invalid log level '%s' in config, defaulting to 'info'", lvlStr) // Use standard log for this bootstrap message
			std.mu.Lock()
			std.level = InfoLevel
			std.mu.Unlock()

		}

	} else {

		std.mu.Lock()
		std.level = InfoLevel // Should not be reached if config defaulting works, but safe fallback
		std.mu.Unlock()

	}

	// Set log format
	formatStr := "text" // Default format
	if pluginLoggingCfg != nil {

		if format, ok := pluginLoggingCfg["format"].(string); ok {
			formatStr = strings.ToLower(format)
		}

	}

	std.mu.Lock() // Lock before modifying the formatter

	switch formatStr {

	case "json":
		std.formatter = &JSONFormatter{}

	case "text":
		std.formatter = &TextFormatter{TimestampFormat: time.RFC3339}

	default:
		// Use standard log for this bootstrap message, as our logger might not be fully set up
		log.Printf("logging: Invalid log format '%s' in config, defaulting to 'text'", formatStr)
		std.formatter = &TextFormatter{TimestampFormat: time.RFC3339}

	}

	std.mu.Unlock()

	// Example: enable caller reporting
	// std.reportCaller = true

	// Initial log message using the configured logger
	std.output(InfoLevel, "Logger initialized", nil)

}

// SetOutput sets the output destination for the standard logger.
func SetOutput(w io.Writer) {

	std.mu.Lock()
	defer std.mu.Unlock()
	std.out = w

}

// SetLevel sets the logging level for the standard logger.
func SetLevel(level Level) {

	std.mu.Lock()
	defer std.mu.Unlock()
	std.level = level

}

// SetFormatter sets the formatter for the standard logger.
func SetFormatter(formatter Formatter) {

	std.mu.Lock()
	defer std.mu.Unlock()
	std.formatter = formatter

}

// SetReportCaller enables or disables reporting of the caller file and line number.
func SetReportCaller(report bool) {

	std.mu.Lock()
	defer std.mu.Unlock()
	std.reportCaller = report

}

func (l *Logger) output(level Level, message string, fields map[string]interface{}) {

	if l.level < level {
		return
	}

	now := time.Now()
	var caller string

	if l.reportCaller {

		_, file, line, ok := runtime.Caller(l.callerSkip)
		if ok {
			caller = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}

	}

	l.mu.Lock()
	defer l.mu.Unlock()

	formattedMsg, err := l.formatter.Format(level, now, message, fields, caller)
	if err != nil {
		// Fallback to simple print if formatting fails
		fmt.Fprintf(l.out, "Error formatting log message: %v. Original: [%s] %s\n", err, levelToString[level], message)
		return
	}

	l.out.Write(formattedMsg)
	if _, ok := l.formatter.(*JSONFormatter); ok { // JSON marshaler doesn't add newline
		l.out.Write([]byte("\n"))
	}

}

// logf is a helper for formatted logging
func (l *Logger) logf(level Level, format string, args ...interface{}) {

	if l.level < level {
		return
	}
	l.output(level, fmt.Sprintf(format, args...), nil)

}

// logw is a helper for logging with fields
func (l *Logger) logw(level Level, message string, fields map[string]interface{}) {

	if l.level < level {
		return
	}
	l.output(level, message, fields)

}

// Debug logs a message at DebugLevel.
func Debug(args ...any) {
	std.output(DebugLevel, fmt.Sprint(args...), nil)
}

// Debugf logs a formatted message at DebugLevel.
func Debugf(format string, args ...any) {
	std.logf(DebugLevel, format, args...)
}

// Debugw logs a message with fields at DebugLevel.
func Debugw(message string, fields map[string]any) {
	std.logw(DebugLevel, message, fields)
}

// Info logs a message at InfoLevel.
func Info(args ...any) {
	std.output(InfoLevel, fmt.Sprint(args...), nil)
}

// Infof logs a formatted message at InfoLevel.
func Infof(format string, args ...any) {
	std.logf(InfoLevel, format, args...)
}

// Infow logs a message with fields at InfoLevel.
func Infow(message string, fields map[string]any) {
	std.logw(InfoLevel, message, fields)
}

// Warn logs a message at WarnLevel.
func Warn(args ...any) {
	std.output(WarnLevel, fmt.Sprint(args...), nil)
}

// Warnf logs a formatted message at WarnLevel.
func Warnf(format string, args ...any) {
	std.logf(WarnLevel, format, args...)
}

// Warnw logs a message with fields at WarnLevel.
func Warnw(message string, fields map[string]any) {
	std.logw(WarnLevel, message, fields)
}

// Error logs a message at ErrorLevel.
func Error(args ...any) {
	std.output(ErrorLevel, fmt.Sprint(args...), nil)
}

// Errorf logs a formatted message at ErrorLevel.
func Errorf(format string, args ...any) {
	std.logf(ErrorLevel, format, args...)
}

// Errorw logs a message with fields at ErrorLevel.
func Errorw(message string, fields map[string]any) {
	std.logw(ErrorLevel, message, fields)
}

// Fatal logs a message at ErrorLevel then calls os.Exit(1).
func Fatal(args ...any) {
	std.output(ErrorLevel, fmt.Sprint(args...), nil)
	os.Exit(1)
}

// Fatalf logs a formatted message at ErrorLevel then calls os.Exit(1).
func Fatalf(format string, args ...any) {
	std.logf(ErrorLevel, format, args...)
	os.Exit(1)
}

// Fatalw logs a message with fields at ErrorLevel then calls os.Exit(1).
func Fatalw(message string, fields map[string]any) {
	std.logw(ErrorLevel, message, fields)
	os.Exit(1)
}

// Panic logs a message at ErrorLevel then panics.
func Panic(args ...any) {
	msg := fmt.Sprint(args...)
	std.output(ErrorLevel, msg, nil)
	panic(msg)
}

// Panicf logs a formatted message at ErrorLevel then panics.
func Panicf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	std.logf(ErrorLevel, "%s", msg) // logf already calls output
	panic(msg)
}

// Panicw logs a message with fields at ErrorLevel then panics.
func Panicw(message string, fields map[string]any) {
	std.logw(ErrorLevel, message, fields)
	panic(fmt.Sprintf("%s %v", message, fields)) // Reconstruct message for panic
}

// GetLogger returns the standard logger instance.
// This is mostly for consistency if we were to allow creating multiple logger instances.
// In this setup, we'd typically just use the package-level functions like Info, Error, etc.
func GetLogger() *Logger {
	return std
}
