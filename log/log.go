package log

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger is service logger
type Logger struct {
	*zap.Logger
}

// With fields to output
func (l *Logger) With(fields ...zap.Field) *Logger {
	return &Logger{
		Logger: l.Logger.With(fields...),
	}
}

// Info is logging info
func (l *Logger) Info(msg string) {
	l.Logger.Info(msg)
}

// Infof is logging info with printf format
func (l *Logger) Infof(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	l.Logger.Info(msg)
}

// Error is logging error
func (l *Logger) Error(msg string) {
	l.Logger.Error(msg)
}

// Errorf is logging error with printf format
func (l *Logger) Errorf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	l.Logger.Error(msg)
}

// Warn is logging warn
func (l *Logger) Warn(msg string) {
	l.Logger.Warn(msg)
}

// Warnf is logging warn with printf format
func (l *Logger) Warnf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	l.Logger.Warn(msg)
}

// Debug is logging debug
func (l *Logger) Debug(msg string, fields ...zap.Field) {
	l.Logger.Debug(msg, fields...)
}

// Debugf is logging debug with printf format
func (l *Logger) Debugf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	l.Logger.Debug(msg)
}

// timeEncoder specifics the time format
func timeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.UTC().Format(time.RFC3339))
}

var (
	defaultLogger *Logger
)

// init init logger with zap
func init() {
	logConfig := zap.NewProductionConfig()
	logConfig.EncoderConfig.EncodeTime = timeEncoder
	logConfig.EncoderConfig.TimeKey = "time"
	logConfig.Level.SetLevel(zap.DebugLevel)
	zapLogger, err := logConfig.Build(zap.AddCallerSkip(2))
	if err != nil {
		panic(err)
	}
	defaultLogger = &Logger{
		Logger: zapLogger,
	}
}

// With fields to output by default logger
func With(fields ...zap.Field) *Logger {
	return &Logger{
		Logger: defaultLogger.Logger.With(fields...),
	}
}

// Info is logging info
func Info(msg string) {
	defaultLogger.Info(msg)
}

// Infof is logging info with printf format
func Infof(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	defaultLogger.Info(msg)
}

// Error is logging error
func Error(msg string) {
	defaultLogger.Error(msg)
}

// Errorf is logging error with printf format
func Errorf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	defaultLogger.Error(msg)
}

// Warn is logging warn
func Warn(msg string) {
	defaultLogger.Warn(msg)
}

// Warnf is logging warn with printf format
func Warnf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	defaultLogger.Warn(msg)
}

// Debug is logging debug
func Debug(msg string, fields ...zap.Field) {
	defaultLogger.Debug(msg, fields...)
}

// Debugf is logging debug with printf format
func Debugf(fmtStr string, msgs ...interface{}) {
	msg := fmt.Sprintf(fmtStr, msgs...)
	defaultLogger.Debug(msg)
}
