package logger

import (
	"log/slog"
	"os"
)

// Logger interface for custom logging
type Logger interface {
	Info(msg string, args ...interface{})
	Error(msg string, args ...interface{})
	Debug(msg string, args ...interface{})
}

// DefaultLogger uses slog for logging
type DefaultLogger struct {
	logger *slog.Logger
}

// NewDefaultLogger creates a new DefaultLogger with slog
func NewDefaultLogger() *DefaultLogger {
	// Customize slog options, e.g., using a text handler for readability
	handler := slog.NewTextHandler(os.Stderr, nil)
	return &DefaultLogger{
		logger: slog.New(handler),
	}
}

// Info logs an informational message
func (l *DefaultLogger) Info(msg string, args ...interface{}) {
	l.logger.Info(msg, args...)
}

// Error logs an error message
func (l *DefaultLogger) Error(msg string, args ...interface{}) {
	l.logger.Error(msg, args...)
}

// Debug logs a debug message
func (l *DefaultLogger) Debug(msg string, args ...interface{}) {
	l.logger.Debug(msg, args...)
}
