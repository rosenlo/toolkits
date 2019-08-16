package log

import (
	"os"
	"sync"

	"github.com/Sirupsen/logrus"
)

const TimeFormatFormat string = "2006-01-02 15:04:05"

var (
	logger     = NewLogger()
	loggerLock = new(sync.RWMutex)
	fields     = logrus.Fields{}
)

func GetLogger() *logrus.Entry {
	return logger
}

func Init(level, appId string) (err error) {
	logrus.SetFormatter(&logrus.TextFormatter{
		DisableColors:   true,
		FullTimestamp:   true,
		TimestampFormat: TimeFormatFormat,
	})
	logrus.SetOutput(os.Stdout)

	switch level {
	case "info":
		logrus.SetLevel(logrus.InfoLevel)
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
	case "warn":
		logrus.SetLevel(logrus.WarnLevel)
	default:
		logrus.Fatal("log conf only allow [info, debug, warn], please check your confguire")
	}
	withField("appId", appId)

	return
}

func SetField(key string, value interface{}) {
	loggerLock.Lock()
	defer loggerLock.Unlock()
	fields[key] = value
	logger = logger.WithFields(fields)
}

func withField(key string, value interface{}) {
	loggerLock.Lock()
	defer loggerLock.Unlock()
	logger.Data[key] = value
}

func NewLogger() *logrus.Entry {
	return logrus.StandardLogger().WithFields(fields)
}

func Clean() {
	logger = NewLogger()
}

func WithField(key string, value interface{}) *logrus.Entry {
	return logger.WithField(key, value)
}

func WithFields(fields map[string]interface{}) *logrus.Entry {
	return logger.WithFields(logrus.Fields(fields))
}

func Println(args ...interface{}) {
	logger.Println(args...)
}

func Printf(format string, args ...interface{}) {
	logger.Printf(format, args...)
}

func Info(args ...interface{}) {
	logger.Info(args...)
}

func Infof(format string, args ...interface{}) {
	logger.Infof(format, args...)
}

func Warn(args ...interface{}) {
	logger.Warn(args...)
}

func Warnf(format string, args ...interface{}) {
	logger.Warnf(format, args...)
}

func Error(args ...interface{}) {
	logger.Error(args...)
}

func Errorf(format string, args ...interface{}) {
	logger.Errorf(format, args...)
}

func Debug(args ...interface{}) {
	logger.Debug(args...)
}

func Debugf(format string, args ...interface{}) {
	logger.Debugf(format, args...)
}

func Fatal(args ...interface{}) {
	logger.Fatal(args...)
}

func Fatalf(format string, args ...interface{}) {
	logger.Fatalf(format, args...)
}
