package log

import (
	"os"
	"sync"

	log "github.com/Sirupsen/logrus"
)

var (
	Logger     *log.Entry
	loggerLock = new(sync.RWMutex)
	fields     = log.Fields{}
)

func GetLogger() *log.Entry {
	return Logger
}

func Init(level, appId string) (err error) {
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)

	switch level {
	case "info":
		log.SetLevel(log.InfoLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	default:
		log.Fatal("log conf only allow [info, debug, warn], please check your confguire")
	}
	SetLoggerField("appId", appId)

	return
}

func SetLoggerField(key string, value interface{}) {
	loggerLock.Lock()
	defer loggerLock.Unlock()
	fields[key] = value
	Logger = log.WithFields(fields)
}

func Println(args ...interface{}) {
	Logger.Println(args...)
}

func Printf(format string, args ...interface{}) {
	Logger.Printf(format, args...)
}

func Info(args ...interface{}) {
	Logger.Info(args...)
}

func Infof(format string, args ...interface{}) {
	Logger.Infof(format, args...)
}

func Error(args ...interface{}) {
	Logger.Error(args...)
}

func Errorf(format string, args ...interface{}) {
	Logger.Errorf(format, args...)
}

func Debug(args ...interface{}) {
	Logger.Debug(args...)
}

func Debugf(format string, args ...interface{}) {
	Logger.Debugf(format, args...)
}
